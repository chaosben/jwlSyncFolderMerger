// Package dbwrite writes a model.DB into a copy of a database created by JW
// Library itself, so schema, triggers and metadata tables stay exactly as the
// app expects them. All ids are assigned fresh and deterministically.
package dbwrite

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/jwlfile"
	"github.com/chaosben/jwlSyncFolderMerger/internal/jwltime"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

// dataTables are emptied before inserting; everything else (LastModified,
// PlaylistItemAccuracy, android_metadata, grdb_migrations) is kept.
var dataTables = []string{
	"TagMap", "PlaylistItemMarkerParagraphMap", "PlaylistItemMarkerBibleVerseMap", "PlaylistItemMarker",
	"PlaylistItemIndependentMediaMap", "PlaylistItemLocationMap", "PlaylistItem", "IndependentMedia",
	"InputField", "Bookmark", "Note", "BlockRange", "UserMark", "Tag", "Location",
}

// Options control details of the written database.
type Options struct {
	Marker       string    // generation id stored in a hidden Location row; "" to omit
	LastModified time.Time // value for the LastModified table
}

// Write creates outPath from templatePath and fills it with db. It returns
// the archive file name to use for each media hash.
func Write(templatePath, outPath string, db *model.DB, opt Options) (map[string]string, error) {
	if err := copyFile(templatePath, outPath); err != nil {
		return nil, err
	}
	conn, err := jwlfile.OpenDB(outPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return nil, err
	}
	w := &writer{db: db, opt: opt}
	if w.cols, err = locationColumns(conn); err != nil {
		return nil, err
	}
	tx, err := conn.Begin()
	if err != nil {
		return nil, err
	}
	w.tx = tx
	if err := w.run(conn); err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, stmt := range []string{"VACUUM", "PRAGMA journal_mode=DELETE"} {
		if _, err := conn.Exec(stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if err := Verify(conn); err != nil {
		return nil, err
	}
	return w.mediaPath, nil
}

// Verify runs SQLite's integrity and foreign key checks.
func Verify(conn *sql.DB) error {
	var res string
	if err := conn.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check: %s", res)
	}
	rows, err := conn.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		rows.Scan(&table, &rowid, &parent, &fkid)
		return fmt.Errorf("foreign_key_check: %s row %d references missing %s", table, rowid.Int64, parent)
	}
	return rows.Err()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func locationColumns(conn *sql.DB) (bool, error) {
	rows, err := conn.Query(`PRAGMA table_info("Location")`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == "Specialty" {
			return true, nil
		}
	}
	return false, rows.Err()
}

type writer struct {
	db        *model.DB
	opt       Options
	tx        *sql.Tx
	cols      bool // Location has Specialty/Edition
	locID     map[model.LocKey]int64
	markID    map[string]int64
	noteID    map[string]int64
	tagID     map[model.TagKey]int64
	mediaID   map[string]int64
	mediaPath map[string]string
	plID      map[string]int64
}

func (w *writer) exec(q string, args ...any) error {
	if _, err := w.tx.Exec(q, args...); err != nil {
		return fmt.Errorf("%s: %w", strings.Fields(q)[0]+" "+strings.Fields(q)[2], err)
	}
	return nil
}

func (w *writer) run(conn *sql.DB) error {
	existing := map[string]bool{}
	rows, err := w.tx.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		existing[n] = true
	}
	rows.Close()
	for _, t := range dataTables {
		if existing[t] {
			if err := w.exec(fmt.Sprintf("DELETE FROM %q", t)); err != nil {
				return err
			}
		}
	}
	steps := []func() error{w.locations, w.userMarks, w.notes, w.tags, w.media, w.playlists, w.tagMaps, w.bookmarks, w.inputFields}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return w.exec("UPDATE LastModified SET LastModified = ?", jwltime.FormatDB(w.opt.LastModified))
}

func (w *writer) locations() error {
	keys := make([]model.LocKey, 0, len(w.db.Locations))
	for k := range w.db.Locations {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	w.locID = map[model.LocKey]int64{}
	unique1 := map[string]int64{} // UNIQUE(BookNumber, ChapterNumber, KeySymbol, MepsLanguage, Type)
	unique2 := map[string]int64{} // IX_Location_Media
	var next int64 = 1
	for _, k := range keys {
		l := w.db.Locations[k]
		u1, u2 := uniqueKeys(l)
		if id, ok := unique1[u1]; ok && u1 != "" {
			w.locID[k] = id
			continue
		}
		if id, ok := unique2[u2]; ok && u2 != "" {
			w.locID[k] = id
			continue
		}
		if err := w.insertLocation(next, l); err != nil {
			return err
		}
		w.locID[k] = next
		if u1 != "" {
			unique1[u1] = next
		}
		if u2 != "" {
			unique2[u2] = next
		}
		next++
	}
	if w.opt.Marker != "" {
		marker := model.Location{
			DocumentID: sql.NullInt64{Int64: 1, Valid: true},
			KeySymbol:  sql.NullString{String: model.MarkerKeySymbol, Valid: true},
			Title:      sql.NullString{String: w.opt.Marker, Valid: true},
		}
		return w.insertLocation(next, marker)
	}
	return nil
}

// uniqueKeys returns the values of both unique constraints on Location, or
// "" where a NULL makes the constraint inapplicable (NULLs are distinct).
func uniqueKeys(l model.Location) (string, string) {
	var u1, u2 string
	if l.BookNumber.Valid && l.ChapterNumber.Valid && l.KeySymbol.Valid && l.MepsLanguage.Valid {
		u1 = model.Canon(l.BookNumber, l.ChapterNumber, l.KeySymbol, l.MepsLanguage, l.Type)
	}
	if l.KeySymbol.Valid && l.MepsLanguage.Valid && l.DocumentID.Valid && l.Track.Valid {
		u2 = model.Canon(l.KeySymbol, l.IssueTagNumber, l.MepsLanguage, l.DocumentID, l.Track, l.Type, l.Specialty.String, l.Edition.String)
	}
	return u1, u2
}

func (w *writer) insertLocation(id int64, l model.Location) error {
	if w.cols {
		return w.exec(`INSERT INTO Location (LocationId, BookNumber, ChapterNumber, DocumentId, Track, IssueTagNumber, KeySymbol, MepsLanguage, Type, Title, Specialty, Edition) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, l.BookNumber, l.ChapterNumber, l.DocumentID, l.Track, l.IssueTagNumber, l.KeySymbol, l.MepsLanguage, l.Type, l.Title, l.Specialty, l.Edition)
	}
	return w.exec(`INSERT INTO Location (LocationId, BookNumber, ChapterNumber, DocumentId, Track, IssueTagNumber, KeySymbol, MepsLanguage, Type, Title) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		id, l.BookNumber, l.ChapterNumber, l.DocumentID, l.Track, l.IssueTagNumber, l.KeySymbol, l.MepsLanguage, l.Type, l.Title)
}

func (w *writer) userMarks() error {
	guids := sortedKeys(w.db.UserMarks)
	w.markID = map[string]int64{}
	var rangeID int64 = 1
	for i, g := range guids {
		m := w.db.UserMarks[g]
		id := int64(i + 1)
		if err := w.exec(`INSERT INTO UserMark (UserMarkId, ColorIndex, LocationId, StyleIndex, UserMarkGuid, Version) VALUES (?,?,?,?,?,?)`,
			id, m.ColorIndex, w.locID[m.Loc], m.StyleIndex, m.GUID, m.Version); err != nil {
			return err
		}
		w.markID[g] = id
		for _, r := range m.Ranges {
			if err := w.exec(`INSERT INTO BlockRange (BlockRangeId, BlockType, Identifier, StartToken, EndToken, UserMarkId) VALUES (?,?,?,?,?,?)`,
				rangeID, r.BlockType, r.Identifier, r.StartToken, r.EndToken, id); err != nil {
				return err
			}
			rangeID++
		}
	}
	return nil
}

func (w *writer) notes() error {
	guids := sortedKeys(w.db.Notes)
	w.noteID = map[string]int64{}
	for i, g := range guids {
		n := w.db.Notes[g]
		id := int64(i + 1)
		var markID, locID sql.NullInt64
		if n.UserMarkGUID.Valid {
			if mid, ok := w.markID[n.UserMarkGUID.String]; ok {
				markID = sql.NullInt64{Int64: mid, Valid: true}
			}
		}
		if n.HasLoc {
			locID = sql.NullInt64{Int64: w.locID[n.Loc], Valid: true}
		}
		if err := w.exec(`INSERT INTO Note (NoteId, Guid, UserMarkId, LocationId, Title, Content, LastModified, Created, BlockType, BlockIdentifier) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, n.GUID, markID, locID, n.Title, n.Content, n.LastModified, n.Created, n.BlockType, n.BlockIdentifier); err != nil {
			return err
		}
		w.noteID[g] = id
	}
	return nil
}

func (w *writer) tags() error {
	tags := make([]model.TagKey, 0, len(w.db.Tags))
	for t := range w.db.Tags {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool {
		if tags[i].Type != tags[j].Type {
			return tags[i].Type < tags[j].Type
		}
		return tags[i].Name < tags[j].Name
	})
	w.tagID = map[model.TagKey]int64{}
	for i, t := range tags {
		id := int64(i + 1)
		if err := w.exec(`INSERT INTO Tag (TagId, Type, Name) VALUES (?,?,?)`, id, t.Type, t.Name); err != nil {
			return err
		}
		w.tagID[t] = id
	}
	return nil
}

func (w *writer) media() error {
	hashes := sortedKeys(w.db.Media)
	w.mediaID = map[string]int64{}
	w.mediaPath = map[string]string{}
	usedPath := map[string]bool{}
	for i, h := range hashes {
		m := w.db.Media[h]
		path := m.FilePath
		if usedPath[path] {
			path = uuidFromHash(h)
		}
		usedPath[path] = true
		id := int64(i + 1)
		if err := w.exec(`INSERT INTO IndependentMedia (IndependentMediaId, OriginalFilename, FilePath, MimeType, Hash) VALUES (?,?,?,?,?)`,
			id, m.OriginalFilename, path, m.MimeType, m.Hash); err != nil {
			return err
		}
		w.mediaID[h] = id
		w.mediaPath[h] = path
	}
	return nil
}

func uuidFromHash(h string) string {
	h = (h + strings.Repeat("0", 32))[:32]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (w *writer) playlists() error {
	sigs := sortedKeys(w.db.Playlists)
	w.plID = map[string]int64{}
	var markerID int64 = 1
	for i, sig := range sigs {
		p := w.db.Playlists[sig]
		id := int64(i + 1)
		var thumb sql.NullString
		if p.ThumbnailHash != "" {
			thumb = sql.NullString{String: w.mediaPath[p.ThumbnailHash], Valid: true}
		}
		if err := w.exec(`INSERT INTO PlaylistItem (PlaylistItemId, Label, StartTrimOffsetTicks, EndTrimOffsetTicks, Accuracy, EndAction, ThumbnailFilePath) VALUES (?,?,?,?,?,?,?)`,
			id, p.Label, p.StartTrimOffsetTicks, p.EndTrimOffsetTicks, p.Accuracy, p.EndAction, thumb); err != nil {
			return err
		}
		w.plID[sig] = id
		seenLoc := map[int64]bool{}
		for _, pl := range p.Locs {
			lid := w.locID[pl.Loc]
			if seenLoc[lid] {
				continue
			}
			seenLoc[lid] = true
			if err := w.exec(`INSERT INTO PlaylistItemLocationMap (PlaylistItemId, LocationId, MajorMultimediaType, BaseDurationTicks) VALUES (?,?,?,?)`,
				id, lid, pl.MajorMultimediaType, pl.BaseDurationTicks); err != nil {
				return err
			}
		}
		for _, pm := range p.Medias {
			if err := w.exec(`INSERT INTO PlaylistItemIndependentMediaMap (PlaylistItemId, IndependentMediaId, DurationTicks) VALUES (?,?,?)`,
				id, w.mediaID[pm.MediaHash], pm.DurationTicks); err != nil {
				return err
			}
		}
		for _, m := range p.Markers {
			if err := w.exec(`INSERT INTO PlaylistItemMarker (PlaylistItemMarkerId, PlaylistItemId, Label, StartTimeTicks, DurationTicks, EndTransitionDurationTicks) VALUES (?,?,?,?,?,?)`,
				markerID, id, m.Label, m.StartTimeTicks, m.DurationTicks, m.EndTransitionDurationTicks); err != nil {
				return err
			}
			for _, v := range m.Verses {
				if err := w.exec(`INSERT INTO PlaylistItemMarkerBibleVerseMap (PlaylistItemMarkerId, VerseId) VALUES (?,?)`, markerID, v); err != nil {
					return err
				}
			}
			for _, pp := range m.Paragraphs {
				if err := w.exec(`INSERT INTO PlaylistItemMarkerParagraphMap (PlaylistItemMarkerId, MepsDocumentId, ParagraphIndex, MarkerIndexWithinParagraph) VALUES (?,?,?,?)`,
					markerID, pp.MepsDocumentID, pp.ParagraphIndex, pp.MarkerIndexWithinParagraph); err != nil {
					return err
				}
			}
			markerID++
		}
	}
	return nil
}

func (w *writer) tagMaps() error {
	list := append([]model.TagMap(nil), w.db.TagMaps...)
	sort.SliceStable(list, func(i, j int) bool {
		ti, tj := w.tagID[list[i].Tag], w.tagID[list[j].Tag]
		if ti != tj {
			return ti < tj
		}
		return list[i].Position < list[j].Position
	})
	seen := map[string]bool{}
	pos := map[int64]int64{}
	var id int64 = 1
	for _, tm := range list {
		tagID, ok := w.tagID[tm.Tag]
		if !ok {
			continue
		}
		var plID, locID, noteID sql.NullInt64
		var target int64
		switch tm.Target.Kind {
		case model.TargetNote:
			target, ok = w.noteID[tm.Target.Note]
			noteID = sql.NullInt64{Int64: target, Valid: true}
		case model.TargetLocation:
			target, ok = w.locID[tm.Target.Loc]
			locID = sql.NullInt64{Int64: target, Valid: true}
		case model.TargetPlaylist:
			target, ok = w.plID[tm.Target.Playlist]
			plID = sql.NullInt64{Int64: target, Valid: true}
		}
		if !ok {
			continue // target is not part of this database
		}
		key := model.Canon(tagID, plID, locID, noteID)
		if seen[key] {
			continue // two locations collapsed onto one row
		}
		seen[key] = true
		if err := w.exec(`INSERT INTO TagMap (TagMapId, PlaylistItemId, LocationId, NoteId, TagId, Position) VALUES (?,?,?,?,?,?)`,
			id, plID, locID, noteID, tagID, pos[tagID]); err != nil {
			return err
		}
		pos[tagID]++
		id++
	}
	return nil
}

func (w *writer) bookmarks() error {
	slots := make([]model.BookmarkSlot, 0, len(w.db.Bookmarks))
	for s := range w.db.Bookmarks {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].PubLoc != slots[j].PubLoc {
			return slots[i].PubLoc.String() < slots[j].PubLoc.String()
		}
		return slots[i].Slot < slots[j].Slot
	})
	seen := map[[2]int64]bool{}
	var id int64 = 1
	for _, s := range slots {
		b := w.db.Bookmarks[s]
		pub := w.locID[s.PubLoc]
		if seen[[2]int64{pub, s.Slot}] {
			continue
		}
		seen[[2]int64{pub, s.Slot}] = true
		if err := w.exec(`INSERT INTO Bookmark (BookmarkId, LocationId, PublicationLocationId, Slot, Title, Snippet, BlockType, BlockIdentifier) VALUES (?,?,?,?,?,?,?,?)`,
			id, w.locID[b.Loc], pub, s.Slot, b.Title, b.Snippet, b.BlockType, b.BlockIdentifier); err != nil {
			return err
		}
		id++
	}
	return nil
}

func (w *writer) inputFields() error {
	keys := make([]model.InputFieldKey, 0, len(w.db.InputFields))
	for k := range w.db.InputFields {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Loc != keys[j].Loc {
			return keys[i].Loc.String() < keys[j].Loc.String()
		}
		return keys[i].TextTag < keys[j].TextTag
	})
	seen := map[string]bool{}
	for _, k := range keys {
		lid := w.locID[k.Loc]
		sk := fmt.Sprint(lid, "\x00", k.TextTag)
		if seen[sk] {
			continue
		}
		seen[sk] = true
		if err := w.exec(`INSERT INTO InputField (LocationId, TextTag, Value) VALUES (?,?,?)`, lid, k.TextTag, w.db.InputFields[k]); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
