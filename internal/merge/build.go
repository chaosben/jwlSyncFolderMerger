package merge

import (
	"database/sql"
	"sort"

	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

const bookmarkSlots = 10 // JW Library shows slots 0-9 per publication

// build assembles the merged database from the chosen unit states and makes
// the result referentially consistent.
func build(chosen map[UnitID]*unit, snaps []*Snapshot, bookmarkLosers []*unit, st *Stats) *model.DB {
	db := model.NewDB()
	for _, s := range snaps {
		if s.DB.SchemaVersion > db.SchemaVersion {
			db.SchemaVersion = s.DB.SchemaVersion
		}
	}
	ids := make([]UnitID, 0, len(chosen))
	for id := range chosen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })

	var members []model.TagMap
	orders := map[model.TagKey][]model.TagMap{}
	for _, id := range ids {
		u := chosen[id]
		switch id.Kind {
		case KindMarks:
			for _, m := range u.marks {
				db.UserMarks[m.GUID] = m
			}
		case KindNote:
			db.Notes[u.note.GUID] = u.note
		case KindTag:
			db.Tags[u.tag] = true
		case KindTagMember:
			members = append(members, u.member)
		case KindTagOrder:
			orders[u.order[0].Tag] = u.order
		case KindBookmark:
			db.Bookmarks[u.slot] = u.bm
		case KindInput:
			db.InputFields[u.inKey] = u.inVal
		case KindPlaylist:
			db.Playlists[u.plSig] = u.pl
		}
	}

	// notes keep their place even if their highlight was dropped
	for g, n := range db.Notes {
		if n.UserMarkGUID.Valid {
			if _, ok := db.UserMarks[n.UserMarkGUID.String]; !ok {
				n.UserMarkGUID = sql.NullString{}
				db.Notes[g] = n
			}
		}
	}

	media := latestMedia(snaps)
	for sig, p := range db.Playlists {
		if p.ThumbnailHash != "" {
			if _, ok := media[p.ThumbnailHash]; !ok {
				p.ThumbnailHash = ""
			}
		}
		kept := p.Medias[:0:0]
		for _, pm := range p.Medias {
			if _, ok := media[pm.MediaHash]; ok {
				kept = append(kept, pm)
			}
		}
		p.Medias = kept
		db.Playlists[sig] = p
		if p.ThumbnailHash != "" {
			db.Media[p.ThumbnailHash] = media[p.ThumbnailHash]
		}
		for _, pm := range p.Medias {
			db.Media[pm.MediaHash] = media[pm.MediaHash]
		}
	}

	buildTagMaps(db, members, orders)
	placeBookmarkLosers(db, bookmarkLosers, st)
	buildLocations(db, snaps)
	return db
}

func latestMedia(snaps []*Snapshot) map[string]model.Media {
	ordered := append([]*Snapshot(nil), snaps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Time.Before(ordered[j].Time) })
	out := map[string]model.Media{}
	for _, s := range ordered {
		for h, m := range s.DB.Media {
			out[h] = m
		}
	}
	return out
}

// buildTagMaps keeps assignments whose target still exists and numbers the
// positions per tag: first in the order of the winning order unit, then any
// remaining items by their original position.
func buildTagMaps(db *model.DB, members []model.TagMap, orders map[model.TagKey][]model.TagMap) {
	byTag := map[model.TagKey]map[string]model.TagMap{}
	for _, tm := range members {
		switch tm.Target.Kind {
		case model.TargetNote:
			if _, ok := db.Notes[tm.Target.Note]; !ok {
				continue
			}
		case model.TargetPlaylist:
			if _, ok := db.Playlists[tm.Target.Playlist]; !ok {
				continue
			}
		}
		if byTag[tm.Tag] == nil {
			byTag[tm.Tag] = map[string]model.TagMap{}
		}
		byTag[tm.Tag][tm.Target.String()] = tm
		db.Tags[tm.Tag] = true // an assignment keeps its tag alive
	}
	tags := make([]model.TagKey, 0, len(byTag))
	for t := range byTag {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool { return tagKeyString(tags[i]) < tagKeyString(tags[j]) })
	for _, t := range tags {
		set := byTag[t]
		var list []model.TagMap
		used := map[string]bool{}
		for _, tm := range orders[t] {
			k := tm.Target.String()
			if m, ok := set[k]; ok && !used[k] {
				list = append(list, m)
				used[k] = true
			}
		}
		var rest []model.TagMap
		for k, tm := range set {
			if !used[k] {
				rest = append(rest, tm)
			}
		}
		sort.Slice(rest, func(i, j int) bool {
			if rest[i].Position != rest[j].Position {
				return rest[i].Position < rest[j].Position
			}
			return rest[i].Target.String() < rest[j].Target.String()
		})
		list = append(list, rest...)
		for i := range list {
			list[i].Position = int64(i)
			db.TagMaps = append(db.TagMaps, list[i])
		}
	}
}

// placeBookmarkLosers moves bookmarks that lost a slot conflict to a free slot
// of the same publication, unless the same place is already bookmarked.
func placeBookmarkLosers(db *model.DB, losers []*unit, st *Stats) {
	for _, u := range losers {
		taken := map[int64]bool{}
		dup := false
		for slot, b := range db.Bookmarks {
			if slot.PubLoc != u.slot.PubLoc {
				continue
			}
			taken[slot.Slot] = true
			if b.Loc == u.bm.Loc && b.BlockType == u.bm.BlockType && b.BlockIdentifier == u.bm.BlockIdentifier {
				dup = true
			}
		}
		if dup {
			continue
		}
		placed := false
		for s := int64(0); s < bookmarkSlots; s++ {
			if !taken[s] {
				db.Bookmarks[model.BookmarkSlot{PubLoc: u.slot.PubLoc, Slot: s}] = u.bm
				st.MovedBookmarks++
				placed = true
				break
			}
		}
		if !placed {
			st.LostBookmarks++
		}
	}
}

// buildLocations adds every referenced location plus all locations known to
// any snapshot (JW Library itself never deletes Location rows). Titles come
// from the newest snapshot that has one.
func buildLocations(db *model.DB, snaps []*Snapshot) {
	ordered := append([]*Snapshot(nil), snaps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Time.Before(ordered[j].Time) })
	for _, s := range ordered {
		for k, l := range s.DB.Locations {
			if prev, ok := db.Locations[k]; ok && !l.Title.Valid {
				l.Title = prev.Title
			}
			db.Locations[k] = l
		}
	}
	need := func(k model.LocKey) {
		if _, ok := db.Locations[k]; !ok {
			db.Locations[k] = locationFromKey(k)
		}
	}
	for _, m := range db.UserMarks {
		need(m.Loc)
	}
	for _, n := range db.Notes {
		if n.HasLoc {
			need(n.Loc)
		}
	}
	for _, tm := range db.TagMaps {
		if tm.Target.Kind == model.TargetLocation {
			need(tm.Target.Loc)
		}
	}
	for slot, b := range db.Bookmarks {
		need(slot.PubLoc)
		need(b.Loc)
	}
	for k := range db.InputFields {
		need(k.Loc)
	}
	for _, p := range db.Playlists {
		for _, pl := range p.Locs {
			need(pl.Loc)
		}
	}
}

func locationFromKey(k model.LocKey) model.Location {
	l := model.Location{
		BookNumber: k.BookNumber, ChapterNumber: k.ChapterNumber, DocumentID: k.DocumentID,
		Track: k.Track, MepsLanguage: k.MepsLanguage, IssueTagNumber: k.IssueTagNumber,
		KeySymbol: k.KeySymbol, Type: k.Type,
	}
	if k.Specialty != "" {
		l.Specialty = sql.NullString{String: k.Specialty, Valid: true}
	}
	if k.Edition != "" {
		l.Edition = sql.NullString{String: k.Edition, Valid: true}
	}
	return l
}
