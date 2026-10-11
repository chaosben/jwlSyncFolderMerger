// Package merge decides, unit by unit, which device state ends up in the
// merged database. A unit is the smallest piece of user data that is merged
// as a whole (a group of connected paragraphs with their highlights, a note,
// a tag assignment, a bookmark slot, ...). Every unit has a canonical state
// string; "absent" is a valid state, which is how deletions are detected.
package merge

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/jwltime"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

type Kind string

const (
	KindMarks     Kind = "marks"     // highlights of a connected paragraph group
	KindNote      Kind = "note"      // one note by GUID
	KindTag       Kind = "tag"       // existence of a tag
	KindTagMember Kind = "tagmember" // a tag assigned to a note/location/playlist item
	KindTagOrder  Kind = "tagorder"  // order of the items of a tag (playlists!)
	KindBookmark  Kind = "bookmark"  // one bookmark slot of a publication
	KindInput     Kind = "input"     // one input field value
	KindPlaylist  Kind = "playlist"  // one playlist item by content
)

type UnitID struct {
	Kind Kind
	Key  string
}

func (u UnitID) String() string { return string(u.Kind) + ":" + u.Key }

// unit is the state of one unit in one snapshot, with the payload needed to
// rebuild it.
type unit struct {
	canon  string
	time   time.Time // intrinsic modification time (notes), zero otherwise
	marks  []model.UserMark
	note   model.Note
	tag    model.TagKey
	member model.TagMap
	order  []model.TagMap
	slot   model.BookmarkSlot
	bm     model.Bookmark
	inKey  model.InputFieldKey
	inVal  string
	pl     model.PlaylistItem
	plSig  string
}

type units map[UnitID]*unit

func canonOf(u *unit) string {
	if u == nil {
		return ""
	}
	return u.canon
}

// grouper joins paragraphs that are covered by a common highlight (in any
// snapshot) into one group, so a highlight is always merged together with all
// paragraphs it touches.
type grouper struct {
	parent map[string]string
	min    map[string]string
}

func newGrouper() *grouper { return &grouper{parent: map[string]string{}} }

func (g *grouper) find(x string) string {
	p, ok := g.parent[x]
	if !ok {
		g.parent[x] = x
		return x
	}
	if p == x {
		return x
	}
	r := g.find(p)
	g.parent[x] = r
	return r
}

func (g *grouper) union(a, b string) {
	ra, rb := g.find(a), g.find(b)
	if ra != rb {
		g.parent[ra] = rb
	}
}

func paragraphKeys(m model.UserMark) []string {
	if len(m.Ranges) == 0 {
		return []string{m.Loc.String() + "#mark:" + m.GUID}
	}
	keys := make([]string, 0, len(m.Ranges))
	for _, r := range m.Ranges {
		keys = append(keys, fmt.Sprintf("%s#%d:%d", m.Loc.String(), r.BlockType, r.Identifier))
	}
	return keys
}

func (g *grouper) add(db *model.DB) {
	for _, m := range db.UserMarks {
		guidNode := "guid:" + m.GUID
		for _, k := range paragraphKeys(m) {
			g.union(guidNode, k)
		}
	}
}

// finish computes a stable group id (the smallest paragraph key) per root.
func (g *grouper) finish() {
	g.min = map[string]string{}
	for x := range g.parent {
		if strings.HasPrefix(x, "guid:") {
			continue
		}
		r := g.find(x)
		if cur, ok := g.min[r]; !ok || x < cur {
			g.min[r] = x
		}
	}
}

func (g *grouper) group(m model.UserMark) string {
	return g.min[g.find("guid:"+m.GUID)]
}

func markCanon(m model.UserMark) string {
	var sb strings.Builder
	sb.WriteString(model.Canon(m.GUID, m.Loc, m.ColorIndex, m.StyleIndex))
	for _, r := range m.Ranges {
		sb.WriteString(model.Canon("", r.BlockType, r.Identifier, r.StartToken, r.EndToken))
	}
	return sb.String()
}

// marksUnit builds a paragraph group state from its highlights.
func marksUnit(marks []model.UserMark) *unit {
	marks = append([]model.UserMark(nil), marks...)
	sort.Slice(marks, func(i, j int) bool { return marks[i].GUID < marks[j].GUID })
	parts := make([]string, len(marks))
	for i, m := range marks {
		parts[i] = markCanon(m)
	}
	return &unit{canon: strings.Join(parts, "\n"), marks: marks}
}

func noteCanon(n model.Note) string {
	return model.Canon(n.Title, n.Content, n.UserMarkGUID, n.HasLoc, n.Loc, n.BlockType, n.BlockIdentifier,
		jwltime.Canonical(n.LastModified), jwltime.Canonical(n.Created))
}

func tagKeyString(t model.TagKey) string { return fmt.Sprintf("%d|%s", t.Type, t.Name) }

// extract splits a database into units.
func extract(db *model.DB, g *grouper) units {
	us := units{}
	for _, m := range db.UserMarks {
		id := UnitID{KindMarks, g.group(m)}
		u := us[id]
		if u == nil {
			u = &unit{}
			us[id] = u
		}
		u.marks = append(u.marks, m)
	}
	for id, u := range us {
		if id.Kind == KindMarks {
			us[id] = marksUnit(u.marks)
		}
	}
	for guid, n := range db.Notes {
		u := &unit{canon: noteCanon(n), note: n}
		if t, err := jwltime.Parse(n.LastModified); err == nil {
			u.time = t
		}
		us[UnitID{KindNote, guid}] = u
	}
	for t := range db.Tags {
		us[UnitID{KindTag, tagKeyString(t)}] = &unit{canon: "1", tag: t}
	}
	orders := map[model.TagKey][]model.TagMap{}
	for _, tm := range db.TagMaps {
		us[UnitID{KindTagMember, tagKeyString(tm.Tag) + "\x1e" + tm.Target.String()}] = &unit{canon: "1", member: tm}
		orders[tm.Tag] = append(orders[tm.Tag], tm)
	}
	for t, list := range orders {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Position != list[j].Position {
				return list[i].Position < list[j].Position
			}
			return list[i].Target.String() < list[j].Target.String()
		})
		parts := make([]string, len(list))
		for i, tm := range list {
			parts[i] = tm.Target.String()
		}
		us[UnitID{KindTagOrder, tagKeyString(t)}] = &unit{canon: strings.Join(parts, "\n"), order: list}
	}
	for slot, b := range db.Bookmarks {
		key := fmt.Sprintf("%s#%d", slot.PubLoc.String(), slot.Slot)
		us[UnitID{KindBookmark, key}] = &unit{
			canon: model.Canon(b.Loc, b.Title, b.Snippet, b.BlockType, b.BlockIdentifier),
			slot:  slot, bm: b,
		}
	}
	for k, v := range db.InputFields {
		us[UnitID{KindInput, k.Loc.String() + "#" + k.TextTag}] = &unit{canon: v, inKey: k, inVal: v}
	}
	for sig, p := range db.Playlists {
		us[UnitID{KindPlaylist, sig}] = &unit{canon: "1", pl: p, plSig: sig}
	}
	return us
}
