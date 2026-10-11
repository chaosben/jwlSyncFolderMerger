package merge

import (
	"fmt"
	"sort"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

// Snapshot is one database state at a point in time (a device backup or a
// merged generation).
type Snapshot struct {
	Name  string
	Time  time.Time // lastModifiedDate of the backup
	DB    *model.DB
	units units
}

// Source is the history of one device: an optional base (the state the
// device is known to have started from) and the backups since, oldest first.
// Without a base the device cannot delete anything.
type Source struct {
	Device string
	Base   *Snapshot
	Chain  []*Snapshot
}

func (s *Source) latest() *Snapshot { return s.Chain[len(s.Chain)-1] }

// Input is everything one merge run looks at.
type Input struct {
	Current *Snapshot // previous merge result, nil on the first run
	Sources []*Source
}

// Decision records a unit that more than one device changed differently.
type Decision struct {
	Unit   UnitID
	Winner string // device name, "" if the unit was deleted
	Others []string
}

type Stats struct {
	Units          int
	Changed        int // units with at least one detected device change
	Conflicts      int // units changed differently by several devices
	Deleted        int // units removed by a detected deletion
	DivergentGroup int // paragraph groups whose highlights differ between device backups
	MovedBookmarks int
	LostBookmarks  int
}

type Result struct {
	DB        *model.DB
	Stats     Stats
	Decisions []Decision
}

type candidate struct {
	src   *Source
	state *unit // nil = absent
	time  time.Time
}

// Merge computes the merged database.
func Merge(in Input) (*Result, error) {
	if len(in.Sources) == 0 {
		return nil, fmt.Errorf("no sources")
	}
	snaps := allSnapshots(in)
	g := newGrouper()
	for _, s := range snaps {
		g.add(s.DB)
	}
	g.finish()
	ids := map[UnitID]bool{}
	for _, s := range snaps {
		s.units = extract(s.DB, g)
		for id := range s.units {
			ids[id] = true
		}
	}
	sorted := make([]UnitID, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })

	res := &Result{}
	chosen := map[UnitID]*unit{}
	var bookmarkLosers []*unit
	for _, id := range sorted {
		res.Stats.Units++
		cands := changes(in.Sources, id)
		var win *unit
		switch {
		case len(cands) > 0:
			res.Stats.Changed++
			sort.SliceStable(cands, func(i, j int) bool { return better(cands[i], cands[j]) })
			win = cands[0].state
			if distinctStates(cands) > 1 {
				res.Stats.Conflicts++
				d := Decision{Unit: id}
				if win != nil {
					d.Winner = cands[0].src.Device
				}
				for _, c := range cands[1:] {
					d.Others = append(d.Others, c.src.Device)
					if id.Kind == KindBookmark && c.state != nil && canonOf(c.state) != canonOf(win) {
						bookmarkLosers = append(bookmarkLosers, c.state)
					}
				}
				res.Decisions = append(res.Decisions, d)
			}
			if win == nil {
				res.Stats.Deleted++
			}
		case in.Current != nil:
			// devices without a base cannot delete or replace, only add
			others := presentUnits(in.Sources, id, true)
			win = in.Current.units[id]
			if id.Kind == KindMarks {
				win = addMarks(win, others)
			} else if win == nil && len(others) > 0 {
				win = others[0]
			}
		default:
			others := presentUnits(in.Sources, id, false)
			if id.Kind == KindMarks {
				win = addMarks(nil, others)
			} else if len(others) > 0 {
				win = others[0]
			}
		}
		if win != nil {
			chosen[id] = win
		}
		if id.Kind == KindMarks && divergent(in.Sources, id) {
			res.Stats.DivergentGroup++
		}
	}
	res.DB = build(chosen, snaps, bookmarkLosers, &res.Stats)
	return res, nil
}

func allSnapshots(in Input) []*Snapshot {
	var out []*Snapshot
	if in.Current != nil {
		out = append(out, in.Current)
	}
	for _, s := range in.Sources {
		if s.Base != nil {
			out = append(out, s.Base)
		}
		out = append(out, s.Chain...)
	}
	return out
}

// changes returns, for every device with a known base, the final state of the
// unit if it differs from the base, together with the estimated change time.
func changes(sources []*Source, id UnitID) []candidate {
	var out []candidate
	for _, s := range sources {
		if s.Base == nil {
			continue
		}
		final := s.latest().units[id]
		if canonOf(final) == canonOf(s.Base.units[id]) {
			continue
		}
		// find where the final state started; the state before it is the
		// last differing one
		start := len(s.Chain) - 1
		for start > 0 && canonOf(s.Chain[start-1].units[id]) == canonOf(final) {
			start--
		}
		var t time.Time
		switch {
		case final == nil:
			// deletion: only known to have happened after the device last had
			// it; using that lower bound lets concurrent edits win
			if start == 0 {
				t = s.Base.Time
			} else {
				t = s.Chain[start-1].Time
			}
		case !final.time.IsZero():
			t = final.time
		default:
			t = s.Chain[start].Time
		}
		out = append(out, candidate{src: s, state: final, time: t})
	}
	return out
}

// better orders candidates: newest change first, present before absent, then
// by canonical state for determinism.
func better(a, b candidate) bool {
	if !a.time.Equal(b.time) {
		return a.time.After(b.time)
	}
	if (a.state != nil) != (b.state != nil) {
		return a.state != nil
	}
	if canonOf(a.state) != canonOf(b.state) {
		return canonOf(a.state) > canonOf(b.state)
	}
	return a.src.Device < b.src.Device
}

func distinctStates(cs []candidate) int {
	seen := map[string]bool{}
	for _, c := range cs {
		key := "absent"
		if c.state != nil {
			key = "present:" + c.state.canon
		}
		seen[key] = true
	}
	return len(seen)
}

// presentUnits returns the unit as found in the latest backup of each device,
// newest first (notes: by modification time). With onlyBaseless, devices with
// a base are skipped: their unchanged units are already reflected in current.
func presentUnits(sources []*Source, id UnitID, onlyBaseless bool) []*unit {
	type entry struct {
		u   *unit
		t   time.Time
		dev string
	}
	var list []entry
	for _, s := range sources {
		if onlyBaseless && s.Base != nil {
			continue
		}
		u := s.latest().units[id]
		if u == nil {
			continue
		}
		t := s.latest().Time
		if !u.time.IsZero() {
			t = u.time
		}
		list = append(list, entry{u, t, s.Device})
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.t.Equal(b.t) {
			return a.t.After(b.t)
		}
		if a.u.canon != b.u.canon {
			return a.u.canon > b.u.canon
		}
		return a.dev < b.dev
	})
	out := make([]*unit, len(list))
	for i, e := range list {
		out[i] = e.u
	}
	return out
}

// addMarks extends a paragraph group with highlights from other states that
// it does not have yet, skipping any that would overlap an existing one.
func addMarks(base *unit, others []*unit) *unit {
	var marks []model.UserMark
	have := map[string]bool{}
	if base != nil {
		marks = append(marks, base.marks...)
		for _, m := range base.marks {
			have[m.GUID] = true
		}
	}
	added := false
	for _, o := range others {
		// highlights of the same device may overlap each other (e.g. an
		// underline on a highlight); only check against other devices
		existing := marks
		var accepted []model.UserMark
		for _, m := range o.marks {
			if have[m.GUID] || overlapsAny(m, existing) {
				continue
			}
			accepted = append(accepted, m)
			have[m.GUID] = true
		}
		if len(accepted) > 0 {
			marks = append(append([]model.UserMark(nil), existing...), accepted...)
			added = true
		}
	}
	if !added {
		return base
	}
	return marksUnit(marks)
}

func overlapsAny(m model.UserMark, list []model.UserMark) bool {
	for _, o := range list {
		if o.Loc != m.Loc {
			continue
		}
		for _, a := range m.Ranges {
			for _, b := range o.Ranges {
				if a.BlockType == b.BlockType && a.Identifier == b.Identifier && tokensOverlap(a, b) {
					return true
				}
			}
		}
	}
	return false
}

// tokensOverlap treats a missing token bound as open-ended.
func tokensOverlap(a, b model.BlockRange) bool {
	aStart, bStart := int64(-1<<62), int64(-1<<62)
	aEnd, bEnd := int64(1<<62), int64(1<<62)
	if a.StartToken.Valid {
		aStart = a.StartToken.Int64
	}
	if b.StartToken.Valid {
		bStart = b.StartToken.Int64
	}
	if a.EndToken.Valid {
		aEnd = a.EndToken.Int64
	}
	if b.EndToken.Valid {
		bEnd = b.EndToken.Int64
	}
	return aStart <= bEnd && bStart <= aEnd
}

func divergent(sources []*Source, id UnitID) bool {
	var first *string
	for _, s := range sources {
		u := s.latest().units[id]
		if u == nil {
			continue
		}
		if first == nil {
			c := u.canon
			first = &c
		} else if *first != u.canon {
			return true
		}
	}
	return false
}
