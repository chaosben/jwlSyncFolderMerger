package model

import (
	"database/sql"
	"fmt"
	"sort"
)

// MinSchema and MaxSchema bound the database versions this package can read.
const (
	MinSchema = 14
	MaxSchema = 16
)

// Load reads a user database. Dangling references (JW Library runs without
// foreign key enforcement) are dropped and recorded in DB.Warnings.
func Load(db *sql.DB) (*DB, error) {
	out := NewDB()
	if err := db.QueryRow("PRAGMA user_version").Scan(&out.SchemaVersion); err != nil {
		return nil, err
	}
	if out.SchemaVersion < MinSchema || out.SchemaVersion > MaxSchema {
		return nil, fmt.Errorf("unsupported schema version %d (supported %d-%d)", out.SchemaVersion, MinSchema, MaxSchema)
	}
	l := loader{db: db, out: out}
	steps := []func() error{
		l.locations, l.userMarks, l.notes, l.tags, l.media, l.playlists,
		l.tagMaps, l.bookmarks, l.inputFields,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type loader struct {
	db        *sql.DB
	out       *DB
	locByID   map[int64]LocKey
	markByID  map[int64]string
	noteByID  map[int64]string
	tagByID   map[int64]TagKey
	mediaByID map[int64]string
	mediaPath map[string]string // FilePath -> hash
	plByID    map[int64]string
}

func (l *loader) warn(format string, args ...any) {
	l.out.Warnings = append(l.out.Warnings, fmt.Sprintf(format, args...))
}

func (l *loader) hasColumn(table, col string) (bool, error) {
	rows, err := l.db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (l *loader) each(query string, fn func(*sql.Rows) error) error {
	rows, err := l.db.Query(query)
	if err != nil {
		return fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (l *loader) locations() error {
	l.locByID = map[int64]LocKey{}
	extra := "NULL, NULL"
	if ok, err := l.hasColumn("Location", "Specialty"); err != nil {
		return err
	} else if ok {
		extra = "Specialty, Edition"
	}
	q := "SELECT LocationId, BookNumber, ChapterNumber, DocumentId, Track, IssueTagNumber, KeySymbol, MepsLanguage, Type, Title, " + extra + " FROM Location"
	return l.each(q, func(r *sql.Rows) error {
		var id int64
		var loc Location
		if err := r.Scan(&id, &loc.BookNumber, &loc.ChapterNumber, &loc.DocumentID, &loc.Track, &loc.IssueTagNumber,
			&loc.KeySymbol, &loc.MepsLanguage, &loc.Type, &loc.Title, &loc.Specialty, &loc.Edition); err != nil {
			return err
		}
		if loc.KeySymbol.String == MarkerKeySymbol {
			l.out.Marker = loc.Title.String
			return nil
		}
		k := loc.Key()
		l.locByID[id] = k
		l.out.Locations[k] = loc
		return nil
	})
}

func (l *loader) userMarks() error {
	l.markByID = map[int64]string{}
	err := l.each("SELECT UserMarkId, ColorIndex, LocationId, StyleIndex, UserMarkGuid, Version FROM UserMark", func(r *sql.Rows) error {
		var id, locID int64
		var m UserMark
		if err := r.Scan(&id, &m.ColorIndex, &locID, &m.StyleIndex, &m.GUID, &m.Version); err != nil {
			return err
		}
		k, ok := l.locByID[locID]
		if !ok {
			l.warn("UserMark %s: missing location %d, dropped", m.GUID, locID)
			return nil
		}
		m.Loc = k
		l.markByID[id] = m.GUID
		l.out.UserMarks[m.GUID] = m
		return nil
	})
	if err != nil {
		return err
	}
	err = l.each("SELECT UserMarkId, BlockType, Identifier, StartToken, EndToken FROM BlockRange", func(r *sql.Rows) error {
		var markID int64
		var br BlockRange
		if err := r.Scan(&markID, &br.BlockType, &br.Identifier, &br.StartToken, &br.EndToken); err != nil {
			return err
		}
		guid, ok := l.markByID[markID]
		if !ok {
			return nil // range of a dropped or missing mark
		}
		m := l.out.UserMarks[guid]
		m.Ranges = append(m.Ranges, br)
		l.out.UserMarks[guid] = m
		return nil
	})
	if err != nil {
		return err
	}
	for g, m := range l.out.UserMarks {
		SortRanges(m.Ranges)
		l.out.UserMarks[g] = m
	}
	return nil
}

func (l *loader) notes() error {
	l.noteByID = map[int64]string{}
	return l.each("SELECT NoteId, Guid, UserMarkId, LocationId, Title, Content, LastModified, Created, BlockType, BlockIdentifier FROM Note", func(r *sql.Rows) error {
		var id int64
		var markID, locID sql.NullInt64
		var n Note
		if err := r.Scan(&id, &n.GUID, &markID, &locID, &n.Title, &n.Content, &n.LastModified, &n.Created, &n.BlockType, &n.BlockIdentifier); err != nil {
			return err
		}
		if markID.Valid {
			if g, ok := l.markByID[markID.Int64]; ok {
				n.UserMarkGUID = sql.NullString{String: g, Valid: true}
			} else {
				l.warn("Note %s: missing user mark %d, unlinked", n.GUID, markID.Int64)
			}
		}
		if locID.Valid {
			if k, ok := l.locByID[locID.Int64]; ok {
				n.HasLoc, n.Loc = true, k
			} else {
				l.warn("Note %s: missing location %d, unlinked", n.GUID, locID.Int64)
			}
		}
		l.noteByID[id] = n.GUID
		l.out.Notes[n.GUID] = n
		return nil
	})
}

func (l *loader) tags() error {
	l.tagByID = map[int64]TagKey{}
	return l.each("SELECT TagId, Type, Name FROM Tag", func(r *sql.Rows) error {
		var id int64
		var t TagKey
		if err := r.Scan(&id, &t.Type, &t.Name); err != nil {
			return err
		}
		l.tagByID[id] = t
		l.out.Tags[t] = true
		return nil
	})
}

func (l *loader) media() error {
	l.mediaByID = map[int64]string{}
	l.mediaPath = map[string]string{}
	return l.each("SELECT IndependentMediaId, OriginalFilename, FilePath, MimeType, Hash FROM IndependentMedia", func(r *sql.Rows) error {
		var id int64
		var m Media
		if err := r.Scan(&id, &m.OriginalFilename, &m.FilePath, &m.MimeType, &m.Hash); err != nil {
			return err
		}
		l.mediaByID[id] = m.Hash
		l.mediaPath[m.FilePath] = m.Hash
		l.out.Media[m.Hash] = m
		return nil
	})
}

func (l *loader) playlists() error {
	items := map[int64]*PlaylistItem{}
	var order []int64
	err := l.each("SELECT PlaylistItemId, Label, StartTrimOffsetTicks, EndTrimOffsetTicks, Accuracy, EndAction, ThumbnailFilePath FROM PlaylistItem", func(r *sql.Rows) error {
		var id int64
		var p PlaylistItem
		var thumb sql.NullString
		if err := r.Scan(&id, &p.Label, &p.StartTrimOffsetTicks, &p.EndTrimOffsetTicks, &p.Accuracy, &p.EndAction, &thumb); err != nil {
			return err
		}
		if thumb.Valid {
			if h, ok := l.mediaPath[thumb.String]; ok {
				p.ThumbnailHash = h
			} else {
				l.warn("PlaylistItem %q: missing thumbnail %s", p.Label, thumb.String)
			}
		}
		items[id] = &p
		order = append(order, id)
		return nil
	})
	if err != nil {
		return err
	}
	err = l.each("SELECT PlaylistItemId, LocationId, MajorMultimediaType, BaseDurationTicks FROM PlaylistItemLocationMap", func(r *sql.Rows) error {
		var id, locID int64
		var pl PlaylistLoc
		if err := r.Scan(&id, &locID, &pl.MajorMultimediaType, &pl.BaseDurationTicks); err != nil {
			return err
		}
		k, ok := l.locByID[locID]
		if p := items[id]; p != nil && ok {
			pl.Loc = k
			p.Locs = append(p.Locs, pl)
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = l.each("SELECT PlaylistItemId, IndependentMediaId, DurationTicks FROM PlaylistItemIndependentMediaMap", func(r *sql.Rows) error {
		var id, mediaID int64
		var pm PlaylistMedia
		if err := r.Scan(&id, &mediaID, &pm.DurationTicks); err != nil {
			return err
		}
		h, ok := l.mediaByID[mediaID]
		if p := items[id]; p != nil && ok {
			pm.MediaHash = h
			p.Medias = append(p.Medias, pm)
		}
		return nil
	})
	if err != nil {
		return err
	}
	markers := map[int64]*PlaylistMarker{}
	markerOwner := map[int64]int64{}
	var markerOrder []int64
	err = l.each("SELECT PlaylistItemMarkerId, PlaylistItemId, Label, StartTimeTicks, DurationTicks, EndTransitionDurationTicks FROM PlaylistItemMarker", func(r *sql.Rows) error {
		var id, owner int64
		var m PlaylistMarker
		if err := r.Scan(&id, &owner, &m.Label, &m.StartTimeTicks, &m.DurationTicks, &m.EndTransitionDurationTicks); err != nil {
			return err
		}
		markers[id] = &m
		markerOwner[id] = owner
		markerOrder = append(markerOrder, id)
		return nil
	})
	if err != nil {
		return err
	}
	err = l.each("SELECT PlaylistItemMarkerId, VerseId FROM PlaylistItemMarkerBibleVerseMap", func(r *sql.Rows) error {
		var id, verse int64
		if err := r.Scan(&id, &verse); err != nil {
			return err
		}
		if m := markers[id]; m != nil {
			m.Verses = append(m.Verses, verse)
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = l.each("SELECT PlaylistItemMarkerId, MepsDocumentId, ParagraphIndex, MarkerIndexWithinParagraph FROM PlaylistItemMarkerParagraphMap", func(r *sql.Rows) error {
		var id int64
		var mp MarkerParagraph
		if err := r.Scan(&id, &mp.MepsDocumentID, &mp.ParagraphIndex, &mp.MarkerIndexWithinParagraph); err != nil {
			return err
		}
		if m := markers[id]; m != nil {
			m.Paragraphs = append(m.Paragraphs, mp)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range markerOrder {
		if p := items[markerOwner[id]]; p != nil {
			m := markers[id]
			sort.Slice(m.Verses, func(i, j int) bool { return m.Verses[i] < m.Verses[j] })
			sort.Slice(m.Paragraphs, func(i, j int) bool { return Canon(m.Paragraphs[i]) < Canon(m.Paragraphs[j]) })
			p.Markers = append(p.Markers, *m)
		}
	}
	l.plByID = map[int64]string{}
	for _, id := range order {
		p := items[id]
		sort.Slice(p.Locs, func(i, j int) bool {
			return Canon(p.Locs[i].Loc, p.Locs[i].MajorMultimediaType) < Canon(p.Locs[j].Loc, p.Locs[j].MajorMultimediaType)
		})
		sort.Slice(p.Medias, func(i, j int) bool { return p.Medias[i].MediaHash < p.Medias[j].MediaHash })
		sort.Slice(p.Markers, func(i, j int) bool { return p.Markers[i].StartTimeTicks < p.Markers[j].StartTimeTicks })
		sig := p.Signature()
		l.plByID[id] = sig
		l.out.Playlists[sig] = *p
	}
	return nil
}

func (l *loader) tagMaps() error {
	return l.each("SELECT PlaylistItemId, LocationId, NoteId, TagId, Position FROM TagMap", func(r *sql.Rows) error {
		var plID, locID, noteID sql.NullInt64
		var tagID int64
		var tm TagMap
		if err := r.Scan(&plID, &locID, &noteID, &tagID, &tm.Position); err != nil {
			return err
		}
		tag, ok := l.tagByID[tagID]
		if !ok {
			l.warn("TagMap: missing tag %d, dropped", tagID)
			return nil
		}
		tm.Tag = tag
		switch {
		case noteID.Valid:
			g, ok := l.noteByID[noteID.Int64]
			if !ok {
				l.warn("TagMap: missing note %d, dropped", noteID.Int64)
				return nil
			}
			tm.Target = Target{Kind: TargetNote, Note: g}
		case locID.Valid:
			k, ok := l.locByID[locID.Int64]
			if !ok {
				l.warn("TagMap: missing location %d, dropped", locID.Int64)
				return nil
			}
			tm.Target = Target{Kind: TargetLocation, Loc: k}
		case plID.Valid:
			s, ok := l.plByID[plID.Int64]
			if !ok {
				l.warn("TagMap: missing playlist item %d, dropped", plID.Int64)
				return nil
			}
			tm.Target = Target{Kind: TargetPlaylist, Playlist: s}
		default:
			return nil
		}
		l.out.TagMaps = append(l.out.TagMaps, tm)
		return nil
	})
}

func (l *loader) bookmarks() error {
	return l.each("SELECT LocationId, PublicationLocationId, Slot, Title, Snippet, BlockType, BlockIdentifier FROM Bookmark", func(r *sql.Rows) error {
		var locID, pubID int64
		var slot int64
		var b Bookmark
		if err := r.Scan(&locID, &pubID, &slot, &b.Title, &b.Snippet, &b.BlockType, &b.BlockIdentifier); err != nil {
			return err
		}
		loc, ok1 := l.locByID[locID]
		pub, ok2 := l.locByID[pubID]
		if !ok1 || !ok2 {
			l.warn("Bookmark %q: missing location, dropped", b.Title)
			return nil
		}
		b.Loc = loc
		l.out.Bookmarks[BookmarkSlot{PubLoc: pub, Slot: slot}] = b
		return nil
	})
}

func (l *loader) inputFields() error {
	return l.each("SELECT LocationId, TextTag, Value FROM InputField", func(r *sql.Rows) error {
		var locID int64
		var tag, val string
		if err := r.Scan(&locID, &tag, &val); err != nil {
			return err
		}
		k, ok := l.locByID[locID]
		if !ok {
			l.warn("InputField %s: missing location %d, dropped", tag, locID)
			return nil
		}
		l.out.InputFields[InputFieldKey{Loc: k, TextTag: tag}] = val
		return nil
	})
}
