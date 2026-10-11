package merge

import (
	"database/sql"
	"testing"

	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

func rng(para, start, end int64) model.BlockRange {
	return model.BlockRange{BlockType: 1, Identifier: para,
		StartToken: sql.NullInt64{Int64: start, Valid: true}, EndToken: sql.NullInt64{Int64: end, Valid: true}}
}

func TestTokensOverlap(t *testing.T) {
	cases := []struct {
		a, b model.BlockRange
		want bool
	}{
		{rng(1, 0, 5), rng(1, 5, 9), true},
		{rng(1, 0, 4), rng(1, 5, 9), false},
		{rng(1, 3, 3), rng(1, 0, 9), true},
		{model.BlockRange{BlockType: 1, Identifier: 1}, rng(1, 5, 9), true},
	}
	for i, c := range cases {
		if got := tokensOverlap(c.a, c.b); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestAddMarksSkipsOverlapsFromOtherDevices(t *testing.T) {
	m := func(g string, r model.BlockRange) model.UserMark {
		return model.UserMark{GUID: g, Ranges: []model.BlockRange{r}}
	}
	base := marksUnit([]model.UserMark{m("a", rng(1, 0, 5))})
	// same device: underline over its own highlight is kept
	other := marksUnit([]model.UserMark{m("b", rng(1, 3, 8)), m("c", rng(1, 10, 12)), m("d", rng(1, 10, 11))})
	got := addMarks(base, []*unit{other})
	have := map[string]bool{}
	for _, x := range got.marks {
		have[x.GUID] = true
	}
	if !have["a"] || have["b"] || !have["c"] || !have["d"] {
		t.Errorf("got %v", have)
	}
}
