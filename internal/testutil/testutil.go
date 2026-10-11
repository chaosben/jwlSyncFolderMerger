// Package testutil builds synthetic .jwlibrary files for tests. It never
// uses real user data.
package testutil

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/dbwrite"
	"github.com/chaosben/jwlSyncFolderMerger/internal/jwlfile"
	"github.com/chaosben/jwlSyncFolderMerger/internal/jwltime"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

// Date returns a UTC time on the given day of September 2026 at hour h.
func Date(day, h int) time.Time { return time.Date(2026, 9, day, h, 0, 0, 0, time.UTC) }

// Doc returns the location key of a document.
func Doc(doc int64) model.LocKey { return docLocation(doc).Key() }

func docLocation(doc int64) model.Location {
	return model.Location{
		DocumentID:   sql.NullInt64{Int64: doc, Valid: true},
		KeySymbol:    sql.NullString{String: "w", Valid: true},
		MepsLanguage: sql.NullInt64{Int64: 2, Valid: true},
	}
}

// Pub returns the location key of a publication (used for bookmarks).
func Pub() model.LocKey { return pubLocation().Key() }

func pubLocation() model.Location {
	return model.Location{
		KeySymbol:    sql.NullString{String: "nwt", Valid: true},
		MepsLanguage: sql.NullInt64{Int64: 2, Valid: true},
		Type:         1,
	}
}

// Builder fills a model.DB.
type Builder struct{ DB *model.DB }

func New() *Builder {
	b := &Builder{DB: model.NewDB()}
	b.DB.SchemaVersion = 16
	l := pubLocation()
	b.DB.Locations[l.Key()] = l
	return b
}

func (b *Builder) doc(doc int64) model.LocKey {
	l := docLocation(doc)
	b.DB.Locations[l.Key()] = l
	return l.Key()
}

// Mark adds a highlight on one paragraph of a document.
func (b *Builder) Mark(guid string, doc, para, start, end, color int64) *Builder {
	b.DB.UserMarks[guid] = model.UserMark{
		GUID: guid, Loc: b.doc(doc), ColorIndex: color,
		Ranges: []model.BlockRange{{BlockType: 1, Identifier: para,
			StartToken: sql.NullInt64{Int64: start, Valid: true}, EndToken: sql.NullInt64{Int64: end, Valid: true}}},
	}
	return b
}

// Note adds a note in a paragraph, optionally attached to a highlight.
func (b *Builder) Note(guid, markGUID string, doc, para int64, content string, modified time.Time) *Builder {
	n := model.Note{
		GUID: guid, HasLoc: true, Loc: b.doc(doc),
		Title:        sql.NullString{String: "T " + guid, Valid: true},
		Content:      sql.NullString{String: content, Valid: true},
		LastModified: jwltime.FormatDB(modified), Created: jwltime.FormatDB(modified),
		BlockType: 1, BlockIdentifier: sql.NullInt64{Int64: para, Valid: true},
	}
	if markGUID != "" {
		n.UserMarkGUID = sql.NullString{String: markGUID, Valid: true}
	}
	b.DB.Notes[guid] = n
	return b
}

// TagNote assigns a tag to a note at a position.
func (b *Builder) TagNote(tag, noteGUID string, pos int64) *Builder {
	k := model.TagKey{Type: 1, Name: tag}
	b.DB.Tags[k] = true
	b.DB.TagMaps = append(b.DB.TagMaps, model.TagMap{Tag: k, Target: model.Target{Kind: model.TargetNote, Note: noteGUID}, Position: pos})
	return b
}

// Bookmark sets a bookmark slot of the test publication to a document.
func (b *Builder) Bookmark(slot, doc int64, title string) *Builder {
	b.DB.Bookmarks[model.BookmarkSlot{PubLoc: Pub(), Slot: slot}] = model.Bookmark{Loc: b.doc(doc), Title: title}
	return b
}

// Clone deep-copies a database so tests can derive device states.
func Clone(src *model.DB) *model.DB {
	db := model.NewDB()
	db.SchemaVersion, db.Marker = src.SchemaVersion, src.Marker
	for k, v := range src.Locations {
		db.Locations[k] = v
	}
	for k, v := range src.UserMarks {
		v.Ranges = append([]model.BlockRange(nil), v.Ranges...)
		db.UserMarks[k] = v
	}
	for k, v := range src.Notes {
		db.Notes[k] = v
	}
	for k, v := range src.Tags {
		db.Tags[k] = v
	}
	db.TagMaps = append(db.TagMaps, src.TagMaps...)
	for k, v := range src.Bookmarks {
		db.Bookmarks[k] = v
	}
	for k, v := range src.InputFields {
		db.InputFields[k] = v
	}
	for k, v := range src.Media {
		db.Media[k] = v
	}
	for k, v := range src.Playlists {
		db.Playlists[k] = v
	}
	return db
}

// WriteJWL writes db as a device backup and sets the file time to modified.
// The generation marker in db.Marker is kept (simulates a restored current).
func WriteJWL(t testing.TB, path, device string, modified time.Time, db *model.DB) {
	t.Helper()
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "template.db")
	conn, err := jwlfile.OpenDB(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(schema16); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	conn.Close()
	out := filepath.Join(dir, "out.db")
	if _, err := dbwrite.Write(tmpl, out, db, dbwrite.Options{Marker: db.Marker, LastModified: modified}); err != nil {
		t.Fatalf("write db: %v", err)
	}
	hash, err := jwlfile.HashFile(out)
	if err != nil {
		t.Fatal(err)
	}
	m := jwlfile.Manifest{
		Name: filepath.Base(path), CreationDate: modified.Format("2006-01-02"), Version: 1,
		UserDataBackup: jwlfile.UserDataBackup{
			LastModifiedDate: jwltime.FormatDB(modified), DeviceName: device,
			DatabaseName: jwlfile.DefaultDBName, Hash: hash, SchemaVersion: 16,
		},
	}
	if err := jwlfile.Write(path, m, out, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// ReadJWL loads the database of a .jwlibrary file.
func ReadJWL(t testing.TB, path string) *model.DB {
	t.Helper()
	f, err := jwlfile.Open(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := jwlfile.OpenDB(f.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	db, err := model.Load(conn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// BackupName returns a file name like the app uses.
func BackupName(device string, t time.Time) string {
	return fmt.Sprintf("UserdataBackup_%s_%s.jwlibrary", t.Format("2006-01-02T15"), device)
}
