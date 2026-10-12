package status

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const name = "jwlsfm_status.html"

var t0 = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func newWriter(t *testing.T, dir string, keep int) *Writer {
	t.Helper()
	w := New(dir, name, filepath.Join(dir, "archive", ".tmp"), keep, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w != nil {
		w.now = func() time.Time { return t0 }
	}
	return w
}

func readPage(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func entry(i int) Entry {
	return Entry{Time: t0.Add(time.Duration(i) * time.Minute), Kind: KindMerged, Text: fmt.Sprintf("event %03d", i)}
}

// Separate writers (one-off runs) continue the same log, newest first, trimmed to keep.
func TestLogContinuesAndIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 7; i++ {
		newWriter(t, dir, 5).Event(entry(i))
	}
	page := readPage(t, dir)
	for i := 1; i <= 7; i++ {
		if got, want := strings.Contains(page, fmt.Sprintf("event %03d", i)), i > 2; got != want {
			t.Errorf("event %d present = %v, want %v", i, got, want)
		}
	}
	if strings.Index(page, "event 007") > strings.Index(page, "event 003") {
		t.Error("newest entry must come first")
	}
}

func TestRunUpdatesHead(t *testing.T) {
	tests := []struct {
		name     string
		run      Run
		contains []string
	}{
		{"merged", Run{Time: t0, Outcome: KindMerged,
			Current: &Current{Time: t0, Generation: "gen-1", Highlights: 12, Notes: 3},
			Devices: []Device{{Name: "Pixel", LastBackup: t0, Restored: "older"}}},
			[]string{`class="banner ok"`, "Everything is up to date", "gen-1", "12 highlights", "Pixel", "restore current.jwlibrary"}},
		{"guard", Run{Time: t0, Outcome: KindGuard, Error: "would remove 40 of 120 highlights"},
			[]string{`class="banner error"`, "safety check", "would remove 40 of 120 highlights"}},
		{"error", Run{Time: t0, Outcome: KindError, Error: "disk full"},
			[]string{`class="banner error"`, "Last run failed", "disk full"}},
		{"pending", Run{Time: t0, Outcome: "nothing-to-do", Pending: 2},
			[]string{`class="banner warning"`, "still being synchronised", "2 backup(s) still syncing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			newWriter(t, dir, 10).Run(tt.run)
			page := readPage(t, dir)
			for _, c := range tt.contains {
				if !strings.Contains(page, c) {
					t.Errorf("page lacks %q", c)
				}
			}
		})
	}
}

// A run that does not know the current state keeps the previous one.
func TestRunKeepsKnownState(t *testing.T) {
	dir := t.TempDir()
	newWriter(t, dir, 10).Run(Run{Time: t0, Outcome: KindMerged, Current: &Current{Generation: "gen-1"},
		Devices: []Device{{Name: "Pixel"}}})
	newWriter(t, dir, 10).Run(Run{Time: t0.Add(time.Hour), Outcome: KindError, Error: "boom"})
	page := readPage(t, dir)
	for _, c := range []string{"gen-1", "Pixel", "boom"} {
		if !strings.Contains(page, c) {
			t.Errorf("page lacks %q", c)
		}
	}
}

func TestEscaping(t *testing.T) {
	dir := t.TempDir()
	evil := `<script>alert(1)</script>`
	w := newWriter(t, dir, 10)
	w.Run(Run{Time: t0, Outcome: KindError, Error: evil, Devices: []Device{{Name: evil}}}, Entry{Time: t0, Kind: KindError, Text: evil})
	page := readPage(t, dir)
	if strings.Contains(page, evil) {
		t.Fatal("unescaped markup in page")
	}
	// the embedded data still round-trips
	if s := w.load(); len(s.Entries) != 1 || s.Entries[0].Text != evil || s.Devices[0].Name != evil {
		t.Fatalf("data did not round-trip: %+v", s)
	}
}

func TestForeignFileStartsOver(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, name), []byte("<html>something else</html>"), 0o644)
	newWriter(t, dir, 10).Event(entry(1))
	if page := readPage(t, dir); !strings.Contains(page, "event 001") {
		t.Fatal("page not written")
	}
}

func TestDisabled(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, 0)
	if w != nil {
		t.Fatal("keep 0 must disable the writer")
	}
	w.SetMode(Mode{Watch: true})
	w.Run(Run{Outcome: KindMerged})
	w.Event(entry(1))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

func TestNoLeftoversAndStaleHint(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, 10)
	w.SetMode(Mode{Watch: true, Since: t0, AutoUpdate: "on", Expect: 6 * time.Hour})
	w.Event(Entry{Time: t0, Kind: KindStart, Text: "Watch mode started"})
	if _, err := os.Stat(filepath.Join(dir, "archive", ".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary directory left behind: %v", err)
	}
	page := readPage(t, dir)
	for _, c := range []string{"watch mode since", "Automatic updates", `Date.parse("2026-10-12T14:00:00Z")`} {
		if !strings.Contains(page, c) {
			t.Errorf("page lacks %q", c)
		}
	}
}

func TestRepeatedEntriesAreSuppressed(t *testing.T) {
	dir := t.TempDir()
	warn := func(at time.Duration) Entry {
		return Entry{Time: t0.Add(at), Kind: KindWarning, Text: "archived device backup unreadable"}
	}
	for _, at := range []time.Duration{0, 6 * time.Hour, 12 * time.Hour, 25 * time.Hour} {
		newWriter(t, dir, 10).Event(warn(at))
	}
	if s := newWriter(t, dir, 10).load(); len(s.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (once per day)", len(s.Entries))
	}
}

func TestStoppedWatchIsNotStale(t *testing.T) {
	dir := t.TempDir()
	w := newWriter(t, dir, 10)
	w.SetMode(Mode{Watch: true, Since: t0, Expect: time.Hour, Stopped: t0.Add(time.Minute)})
	w.Event(Entry{Time: t0, Kind: KindStop, Text: "Watch mode stopped"})
	page := readPage(t, dir)
	if !strings.Contains(page, ", stopped ") || strings.Contains(page, "Date.parse") {
		t.Fatal("a cleanly stopped watch mode must be shown as stopped, without stale check")
	}
}

func TestRepeatedStartsAreKept(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		newWriter(t, dir, 10).Event(Entry{Time: t0.Add(time.Duration(i) * time.Minute), Kind: KindStart, Text: "Watch mode started, version 1.2.3"})
	}
	if s := newWriter(t, dir, 10).load(); len(s.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(s.Entries))
	}
}
