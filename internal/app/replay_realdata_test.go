//go:build realdata

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/jwlfile"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
)

// TestReplayRealHistory feeds real backups (from the comma separated
// directories in $JWLSFM_HISTORY) one by one, in chronological order, into
// a fresh folder and runs a merge after each one. It never touches the
// source directories.
//
// Run with: JWLSFM_HISTORY=/dir1,/dir2 go test -tags realdata -run Replay -v ./internal/app
func TestReplayRealHistory(t *testing.T) {
	env := os.Getenv("JWLSFM_HISTORY")
	if env == "" {
		t.Skip("JWLSFM_HISTORY not set")
	}
	type item struct {
		path   string
		time   time.Time
		device string
	}
	var items []item
	seen := map[string]bool{}
	scan := t.TempDir()
	for _, dir := range strings.Split(env, ",") {
		files, _ := filepath.Glob(filepath.Join(dir, "*.jwlibrary"))
		for _, p := range files {
			f, err := jwlfile.Open(p, scan)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if !seen[f.DBHash] {
				seen[f.DBHash] = true
				items = append(items, item{p, f.Modified, f.Device()})
			}
			f.Close()
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].time.Before(items[j].time) })
	t.Logf("%d distinct backups", len(items))

	root := t.TempDir()
	var lastDevice *model.DB
	for i, it := range items {
		dst := filepath.Join(root, filepath.Base(it.path))
		copyForTest(t, it.path, dst)
		old := it.time.Add(-time.Hour)
		os.Chtimes(dst, old, old)
		rep, err := Run(context.Background(), Config{
			Dir: root, MinAge: time.Minute, Sync: "syncthing", WorkDir: t.TempDir(),
			Now: func() time.Time { return it.time.Add(time.Minute) },
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("step %d (%s %s): %v", i, it.time.Format("2006-01-02"), it.device, err)
		}
		cur := loadCurrent(t, root)
		dev := loadFile(t, it.path)
		extra, missing := diffMarks(cur, dev)
		t.Logf("%2d %s %-16s %-13s changed=%4d conflicts=%2d deleted=%3d removedMarks=%3d removedNotes=%2d | current marks=%5d notes=%4d | vs this backup: +%d -%d",
			i, it.time.Format("2006-01-02"), it.device, rep.Outcome, rep.Stats.Changed, rep.Stats.Conflicts,
			rep.Stats.Deleted, rep.Loss.Marks, rep.Loss.Notes, len(cur.UserMarks), len(cur.Notes), extra, missing)
		lastDevice = dev
	}
	cur := loadCurrent(t, root)
	extra, missing := diffMarks(cur, lastDevice)
	t.Logf("final: current has %d highlights not in the newest backup, misses %d of it", extra, missing)
	if missing > 0 {
		t.Errorf("current misses %d highlights of the newest device backup", missing)
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.jwlibrary"))
	if len(files) != 1 {
		t.Errorf("expected only current.jwlibrary in root, got %v", files)
	}
}

func copyForTest(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(out, in)
	out.Close()
}

func loadFile(t *testing.T, path string) *model.DB {
	t.Helper()
	f, err := jwlfile.Open(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conn, _ := jwlfile.OpenDB(f.DBPath)
	defer conn.Close()
	db, err := model.Load(conn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func loadCurrent(t *testing.T, root string) *model.DB {
	return loadFile(t, filepath.Join(root, "current.jwlibrary"))
}

func diffMarks(cur, dev *model.DB) (extra, missing int) {
	for g := range cur.UserMarks {
		if _, ok := dev.UserMarks[g]; !ok {
			extra++
		}
	}
	for g := range dev.UserMarks {
		if _, ok := cur.UserMarks[g]; !ok {
			missing++
		}
	}
	return
}
