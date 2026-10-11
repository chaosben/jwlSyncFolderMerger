package merge

import "github.com/chaosben/jwlSyncFolderMerger/internal/model"

// SameContent reports whether two databases hold the same user data
// (ignoring ids, unreferenced locations and the generation marker).
func SameContent(a, b *model.DB) bool {
	g := newGrouper()
	g.add(a)
	g.add(b)
	g.finish()
	ua, ub := extract(a, g), extract(b, g)
	if len(ua) != len(ub) {
		return false
	}
	for id, u := range ua {
		if canonOf(ub[id]) != u.canon {
			return false
		}
	}
	return true
}

// Loss describes how much of a reference database is missing in a result.
type Loss struct {
	Marks, Notes         int
	RefMarks, RefNotes   int
	MarkRatio, NoteRatio float64
}

// Removed counts highlights and notes of ref that are no longer in res.
func Removed(ref, res *model.DB) Loss {
	var l Loss
	l.RefMarks, l.RefNotes = len(ref.UserMarks), len(ref.Notes)
	for g := range ref.UserMarks {
		if _, ok := res.UserMarks[g]; !ok {
			l.Marks++
		}
	}
	for g := range ref.Notes {
		if _, ok := res.Notes[g]; !ok {
			l.Notes++
		}
	}
	if l.RefMarks > 0 {
		l.MarkRatio = float64(l.Marks) / float64(l.RefMarks)
	}
	if l.RefNotes > 0 {
		l.NoteRatio = float64(l.Notes) / float64(l.RefNotes)
	}
	return l
}
