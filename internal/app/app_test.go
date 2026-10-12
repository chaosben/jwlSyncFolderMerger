package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/folder"
	"github.com/chaosben/jwlSyncFolderMerger/internal/merge"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
	"github.com/chaosben/jwlSyncFolderMerger/internal/status"
	tu "github.com/chaosben/jwlSyncFolderMerger/internal/testutil"
)

type env struct {
	t   *testing.T
	dir string
}

func newEnv(t *testing.T) *env { return &env{t: t, dir: t.TempDir()} }

func (e *env) put(device string, at time.Time, db *model.DB) string {
	p := filepath.Join(e.dir, tu.BackupName(device, at))
	tu.WriteJWL(e.t, p, device, at, db)
	return p
}

func (e *env) run(now time.Time, mod ...func(*Config)) (*Report, error) {
	cfg := Config{
		Dir: e.dir, MinAge: time.Minute, Sync: "syncthing", WorkDir: e.t.TempDir(),
		Now: func() time.Time { return now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, m := range mod {
		m(&cfg)
	}
	return Run(context.Background(), cfg)
}

func (e *env) mustRun(now time.Time, mod ...func(*Config)) *Report {
	e.t.Helper()
	rep, err := e.run(now, mod...)
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return rep
}

func (e *env) current() *model.DB {
	return tu.ReadJWL(e.t, filepath.Join(e.dir, "current.jwlibrary"))
}

func (e *env) incoming() []string {
	files, _ := filepath.Glob(filepath.Join(e.dir, "UserdataBackup_*.jwlibrary"))
	return files
}

// seed creates two devices with disjoint data and merges them once.
func seed(t *testing.T) *env {
	e := newEnv(t)
	a := tu.New().Mark("m1", 1, 1, 0, 5, 1).Mark("m2", 1, 2, 0, 5, 1).
		Note("n1", "m1", 1, 1, "note one", tu.Date(1, 8)).TagNote("study", "n1", 0).Bookmark(0, 1, "doc1")
	b := tu.New().Mark("m3", 2, 1, 0, 9, 2).Note("n2", "", 2, 1, "note two", tu.Date(2, 8)).TagNote("study", "n2", 0)
	e.put("A", tu.Date(1, 10), a.DB)
	e.put("B", tu.Date(2, 10), b.DB)
	e.mustRun(tu.Date(3, 0))
	return e
}

func TestFirstRunUnion(t *testing.T) {
	e := seed(t)
	cur := e.current()
	for _, g := range []string{"m1", "m2", "m3"} {
		if _, ok := cur.UserMarks[g]; !ok {
			t.Errorf("missing highlight %s", g)
		}
	}
	if len(cur.Notes) != 2 || cur.Marker == "" {
		t.Errorf("notes=%d marker=%q", len(cur.Notes), cur.Marker)
	}
	if len(e.incoming()) != 0 {
		t.Errorf("incoming not cleaned: %v", e.incoming())
	}
	for _, d := range []string{"A", "B"} {
		if _, err := os.Stat(filepath.Join(e.dir, "archive", "devices", d+".jwlibrary")); err != nil {
			t.Errorf("device base %s not archived", d)
		}
	}
	// tag positions are numbered without gaps
	pos := map[int64]bool{}
	for _, tm := range cur.TagMaps {
		pos[tm.Position] = true
	}
	if !pos[0] || !pos[1] || len(pos) != 2 {
		t.Errorf("tag positions %v", pos)
	}
}

func TestOrderIndependent(t *testing.T) {
	build := func(nameA, nameB string) *model.DB {
		e := newEnv(t)
		a := tu.New().Mark("m1", 1, 1, 0, 5, 1).Note("n1", "", 1, 1, "x", tu.Date(1, 8))
		b := tu.New().Mark("m9", 1, 1, 6, 9, 3).Note("n1", "", 1, 1, "y", tu.Date(1, 9))
		pa := filepath.Join(e.dir, nameA)
		pb := filepath.Join(e.dir, nameB)
		tu.WriteJWL(t, pa, "A", tu.Date(1, 10), a.DB)
		tu.WriteJWL(t, pb, "B", tu.Date(1, 10), b.DB)
		e.mustRun(tu.Date(3, 0))
		return e.current()
	}
	x := build("a.jwlibrary", "b.jwlibrary")
	y := build("z.jwlibrary", "c.jwlibrary")
	if !merge.SameContent(x, y) {
		t.Error("result depends on file names/order")
	}
	if x.Notes["n1"].Content.String != "y" {
		t.Errorf("newer note version should win, got %q", x.Notes["n1"].Content.String)
	}
}

func TestNothingNewChangesNothing(t *testing.T) {
	e := seed(t)
	before, _ := os.ReadFile(filepath.Join(e.dir, "current.jwlibrary"))
	rep := e.mustRun(tu.Date(4, 0))
	after, _ := os.ReadFile(filepath.Join(e.dir, "current.jwlibrary"))
	if rep.Outcome != NothingToDo || string(before) != string(after) {
		t.Errorf("outcome %s, current changed=%v", rep.Outcome, string(before) != string(after))
	}
}

func TestRestoredDeviceDeletes(t *testing.T) {
	e := seed(t)
	restored := tu.Clone(e.current())
	delete(restored.UserMarks, "m3")
	delete(restored.Notes, "n2")
	e.put("A", tu.Date(5, 10), restored)
	// B sends an unchanged backup at the same time
	b := tu.New().Mark("m3", 2, 1, 0, 9, 2).Note("n2", "", 2, 1, "note two", tu.Date(2, 8)).TagNote("study", "n2", 0)
	e.put("B", tu.Date(5, 11), b.DB)
	e.mustRun(tu.Date(6, 0))
	cur := e.current()
	if _, ok := cur.UserMarks["m3"]; ok {
		t.Error("highlight deleted on A came back")
	}
	if _, ok := cur.Notes["n2"]; ok {
		t.Error("note deleted on A came back")
	}
	if _, ok := cur.UserMarks["m1"]; !ok {
		t.Error("unrelated highlight lost")
	}
}

func TestDeviceWithoutRestoreDeletesNothing(t *testing.T) {
	e := seed(t)
	// A never restored current: it still only knows its own data, plus one new highlight
	a := tu.New().Mark("m1", 1, 1, 0, 5, 1).Mark("m2", 1, 2, 0, 5, 1).Mark("m4", 3, 1, 0, 2, 4).
		Note("n1", "m1", 1, 1, "note one", tu.Date(1, 8)).TagNote("study", "n1", 0).Bookmark(0, 1, "doc1")
	e.put("A", tu.Date(5, 10), a.DB)
	// a brand-new device C with partial data
	c := tu.New().Mark("m1", 1, 1, 0, 5, 1)
	e.put("C", tu.Date(5, 11), c.DB)
	e.mustRun(tu.Date(6, 0))
	cur := e.current()
	for _, g := range []string{"m1", "m2", "m3", "m4"} {
		if _, ok := cur.UserMarks[g]; !ok {
			t.Errorf("highlight %s missing", g)
		}
	}
	if len(cur.Notes) != 2 {
		t.Errorf("notes = %d", len(cur.Notes))
	}
}

func TestEarlierChangeBeatsUntouchedNewerBackup(t *testing.T) {
	e := seed(t)
	gen := e.current()
	// B recolours m1 early
	b := tu.Clone(gen)
	m := b.UserMarks["m1"]
	m.ColorIndex = 6
	b.UserMarks["m1"] = m
	e.put("B", tu.Date(5, 8), b)
	// A works elsewhere later, m1 untouched
	a := tu.Clone(gen)
	a.UserMarks["m5"] = model.UserMark{GUID: "m5", Loc: tu.Doc(1), ColorIndex: 2,
		Ranges: []model.BlockRange{{BlockType: 1, Identifier: 7}}}
	e.put("A", tu.Date(6, 8), a)
	e.mustRun(tu.Date(7, 0))
	cur := e.current()
	if cur.UserMarks["m1"].ColorIndex != 6 {
		t.Errorf("m1 colour = %d, want B's change 6", cur.UserMarks["m1"].ColorIndex)
	}
	if _, ok := cur.UserMarks["m5"]; !ok {
		t.Error("A's new highlight missing")
	}
}

func TestSameHighlightChangedOnTwoDevices(t *testing.T) {
	e := seed(t)
	gen := e.current()
	change := func(db *model.DB, start, end int64, extra string) {
		m := db.UserMarks["m1"]
		m.Ranges = []model.BlockRange{{BlockType: 1, Identifier: 1}}
		m.Ranges[0].StartToken.Int64, m.Ranges[0].StartToken.Valid = start, true
		m.Ranges[0].EndToken.Int64, m.Ranges[0].EndToken.Valid = end, true
		db.UserMarks["m1"] = m
		// a second highlight in the same paragraph
		db.UserMarks[extra] = model.UserMark{GUID: extra, Loc: tu.Doc(1), ColorIndex: 3,
			Ranges: []model.BlockRange{{BlockType: 1, Identifier: 1}}}
	}
	a := tu.Clone(gen)
	change(a, 2, 4, "xa")
	e.put("A", tu.Date(5, 8), a)
	b := tu.Clone(gen)
	change(b, 1, 3, "xb")
	e.put("B", tu.Date(6, 8), b)
	rep := e.mustRun(tu.Date(7, 0))
	cur := e.current()
	if got := cur.UserMarks["m1"].Ranges[0].StartToken.Int64; got != 1 {
		t.Errorf("m1 start = %d, want newer change from B", got)
	}
	_, hasA := cur.UserMarks["xa"]
	_, hasB := cur.UserMarks["xb"]
	if hasA || !hasB {
		t.Errorf("paragraph mixed: xa=%v xb=%v", hasA, hasB)
	}
	if rep.Stats.Conflicts == 0 {
		t.Error("conflict not reported")
	}
}

func TestNoteConflictUsesModificationTime(t *testing.T) {
	e := seed(t)
	gen := e.current()
	edit := func(db *model.DB, text string, at time.Time) {
		n := db.Notes["n1"]
		n.Content.String = text
		n.LastModified = at.Format(time.RFC3339)
		db.Notes["n1"] = n
	}
	a := tu.Clone(gen)
	edit(a, "edited on A", tu.Date(5, 8))
	e.put("A", tu.Date(9, 8), a) // backed up late
	b := tu.Clone(gen)
	edit(b, "edited on B", tu.Date(6, 8))
	e.put("B", tu.Date(7, 8), b)
	e.mustRun(tu.Date(10, 0))
	if got := e.current().Notes["n1"].Content.String; got != "edited on B" {
		t.Errorf("note = %q, want the later edit from B", got)
	}
}

func TestBookmarkSlotConflictMovesLoser(t *testing.T) {
	e := seed(t)
	gen := e.current()
	a := tu.Clone(gen)
	a.Bookmarks[model.BookmarkSlot{PubLoc: tu.Pub(), Slot: 0}] = model.Bookmark{Loc: tu.Doc(2), Title: "from A"}
	e.put("A", tu.Date(5, 8), a)
	b := tu.Clone(gen)
	b.Bookmarks[model.BookmarkSlot{PubLoc: tu.Pub(), Slot: 0}] = model.Bookmark{Loc: tu.Doc(3), Title: "from B"}
	b.Locations[tu.Doc(3)] = model.Location{DocumentID: tu.Doc(3).DocumentID, KeySymbol: tu.Doc(3).KeySymbol, MepsLanguage: tu.Doc(3).MepsLanguage}
	e.put("B", tu.Date(6, 8), b)
	rep := e.mustRun(tu.Date(7, 0))
	cur := e.current()
	if cur.Bookmarks[model.BookmarkSlot{PubLoc: tu.Pub(), Slot: 0}].Title != "from B" {
		t.Error("newer bookmark should keep the slot")
	}
	if cur.Bookmarks[model.BookmarkSlot{PubLoc: tu.Pub(), Slot: 1}].Title != "from A" {
		t.Errorf("older bookmark should move to slot 1: %+v", cur.Bookmarks)
	}
	if rep.Stats.MovedBookmarks != 1 {
		t.Errorf("moved = %d", rep.Stats.MovedBookmarks)
	}
}

func TestNoteSurvivesDroppedHighlight(t *testing.T) {
	e := seed(t)
	gen := e.current()
	a := tu.Clone(gen)
	a.Notes["n9"] = model.Note{GUID: "n9", HasLoc: true, Loc: tu.Doc(1), LastModified: "2026-09-05T08:00:00Z", Created: "2026-09-05T08:00:00Z",
		UserMarkGUID: gen.Notes["n1"].UserMarkGUID, Content: gen.Notes["n1"].Content}
	e.put("A", tu.Date(5, 8), a)
	b := tu.Clone(gen)
	delete(b.UserMarks, "m1")
	e.put("B", tu.Date(6, 8), b)
	e.mustRun(tu.Date(7, 0))
	cur := e.current()
	if _, ok := cur.UserMarks["m1"]; ok {
		t.Error("m1 should be deleted")
	}
	n, ok := cur.Notes["n9"]
	if !ok || n.UserMarkGUID.Valid || !n.HasLoc {
		t.Errorf("note on dropped highlight: present=%v mark=%v loc=%v", ok, n.UserMarkGUID, n.HasLoc)
	}
}

func TestMassDeletionAborts(t *testing.T) {
	e := newEnv(t)
	b := tu.New()
	for i := int64(0); i < 60; i++ {
		b.Mark("g"+string(rune('A'+i%26))+string(rune('a'+i/26)), 1, i+1, 0, 3, 1)
	}
	e.put("A", tu.Date(1, 10), b.DB)
	e.mustRun(tu.Date(2, 0))
	before, _ := os.ReadFile(filepath.Join(e.dir, "current.jwlibrary"))
	wiped := tu.Clone(e.current())
	for g := range wiped.UserMarks {
		delete(wiped.UserMarks, g)
		if len(wiped.UserMarks) == 5 {
			break
		}
	}
	p := e.put("A", tu.Date(3, 10), wiped)
	_, err := e.run(tu.Date(4, 0))
	if !errors.Is(err, ErrGuard) {
		t.Fatalf("expected guard error, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(e.dir, "current.jwlibrary"))
	if string(before) != string(after) {
		t.Error("current changed despite guard")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("incoming backup must stay after abort")
	}
}

func TestRecentFileIsLeftAlone(t *testing.T) {
	e := seed(t)
	p := e.put("A", tu.Date(5, 10), tu.New().Mark("m7", 4, 1, 0, 1, 1).DB)
	rep := e.mustRun(tu.Date(5, 10).Add(30 * time.Second))
	if rep.Outcome != NothingToDo {
		t.Errorf("outcome = %s", rep.Outcome)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("syncing file was touched")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t)
	p := e.put("A", tu.Date(1, 10), tu.New().Mark("m1", 1, 1, 0, 1, 1).DB)
	rep := e.mustRun(tu.Date(2, 0), func(c *Config) { c.DryRun = true })
	if rep.Outcome != Merged || len(rep.Actions) == 0 {
		t.Errorf("outcome %s actions %v", rep.Outcome, rep.Actions)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "current.jwlibrary")); err == nil {
		t.Error("dry run wrote current")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("dry run moved the backup")
	}
}

func TestDuplicateConflictCopyRemoved(t *testing.T) {
	e := newEnv(t)
	db := tu.New().Mark("m1", 1, 1, 0, 1, 1).DB
	p := e.put("A", tu.Date(1, 10), db)
	dup := filepath.Join(e.dir, "UserdataBackup_A.sync-conflict-20260901-101010-ABCDEFG.jwlibrary")
	raw, _ := os.ReadFile(p)
	os.WriteFile(dup, raw, 0o644)
	os.Chtimes(dup, tu.Date(1, 10), tu.Date(1, 10))
	e.mustRun(tu.Date(2, 0))
	if files := e.incoming(); len(files) != 0 {
		t.Errorf("left over: %v", files)
	}
	if _, err := os.Stat(dup); err == nil {
		t.Error("duplicate not removed")
	}
}

func TestGenerationsPruned(t *testing.T) {
	e := newEnv(t)
	db := tu.New()
	for i := 1; i <= 5; i++ {
		db.Mark("m"+string(rune('0'+i)), int64(i), 1, 0, 1, 1)
		e.put("A", tu.Date(i, 10), tu.Clone(db.DB))
		e.mustRun(tu.Date(i, 12), func(c *Config) { c.KeepGenerations = 2 })
	}
	gens, _ := filepath.Glob(filepath.Join(e.dir, "archive", "generations", "*.jwlibrary"))
	if len(gens) != 2 {
		t.Errorf("generations = %d, want 2", len(gens))
	}
	if len(e.current().UserMarks) != 5 {
		t.Errorf("current marks = %d", len(e.current().UserMarks))
	}
}

func (e *env) withStatus(c *Config) {
	lay := folder.Layout{Root: e.dir}
	c.Status = status.New(e.dir, folder.StatusName, lay.TempDir(), 50, "test", c.Log)
}

func (e *env) statusPage() string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, folder.StatusName))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// devicesRow returns the table row of a device on the status page.
func devicesRow(page, device string) string {
	i := strings.Index(page, "<tr><td>"+device+"</td>")
	if i < 0 {
		return ""
	}
	return page[i : i+strings.Index(page[i:], "</tr>")]
}

func TestStatusPage(t *testing.T) {
	e := newEnv(t)
	e.put("A", tu.Date(1, 10), tu.New().Mark("m1", 1, 1, 0, 5, 1).DB)
	e.put("B", tu.Date(2, 10), tu.New().Mark("m2", 2, 1, 0, 9, 2).DB)
	e.mustRun(tu.Date(3, 0), e.withStatus)
	page := e.statusPage()
	for _, want := range []string{"Merged new backups of A, B", `class="banner ok"`, "2 highlights"} {
		if !strings.Contains(page, want) {
			t.Errorf("after merge: page lacks %q", want)
		}
	}
	if row := devicesRow(page, "A"); !strings.Contains(row, "never") {
		t.Errorf("A has not restored current yet: %q", row)
	}
	events := strings.Count(page, `<tr class="k-`)

	// nothing new: only the head changes
	e.mustRun(tu.Date(4, 0), e.withStatus)
	if got := strings.Count(e.statusPage(), `<tr class="k-`); got != events {
		t.Errorf("events after a run without new backups = %d, want %d", got, events)
	}

	// A restores current and sends a backup of it
	e.put("A", tu.Date(5, 10), tu.Clone(e.current()))
	e.mustRun(tu.Date(6, 0), e.withStatus)
	page = e.statusPage()
	if row := devicesRow(page, "A"); !strings.Contains(row, "yes") {
		t.Errorf("A restored current: %q", row)
	}
	if row := devicesRow(page, "B"); !strings.Contains(row, "never") {
		t.Errorf("B has not restored current: %q", row)
	}
	if !strings.Contains(page, "Processed new backups of A") {
		t.Error("unchanged run not logged")
	}

	// dry runs never write
	os.Remove(filepath.Join(e.dir, folder.StatusName))
	e.put("B", tu.Date(7, 10), tu.New().Mark("m2", 2, 1, 0, 9, 2).Mark("m3", 3, 1, 0, 1, 1).DB)
	e.mustRun(tu.Date(8, 0), e.withStatus, func(c *Config) { c.DryRun = true })
	if _, err := os.Stat(filepath.Join(e.dir, folder.StatusName)); !os.IsNotExist(err) {
		t.Errorf("dry run wrote the status page: %v", err)
	}
}

func TestStatusPageShowsGuard(t *testing.T) {
	e := newEnv(t)
	b := tu.New()
	for i := int64(0); i < 60; i++ {
		b.Mark("g"+string(rune('A'+i%26))+string(rune('a'+i/26)), 1, i+1, 0, 3, 1)
	}
	e.put("A", tu.Date(1, 10), b.DB)
	e.mustRun(tu.Date(2, 0), e.withStatus)
	wiped := tu.Clone(e.current())
	for g := range wiped.UserMarks {
		delete(wiped.UserMarks, g)
		if len(wiped.UserMarks) == 5 {
			break
		}
	}
	e.put("A", tu.Date(3, 10), wiped)
	if _, err := e.run(tu.Date(4, 0), e.withStatus); !errors.Is(err, ErrGuard) {
		t.Fatalf("expected guard error, got %v", err)
	}
	page := e.statusPage()
	for _, want := range []string{`class="banner error"`, "Aborted, nothing changed", "60 highlights"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}
