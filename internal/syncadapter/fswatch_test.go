package syncadapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnWSLDrive(t *testing.T) {
	cases := []struct {
		path string
		wsl  bool
		want bool
	}{
		{"/mnt/c/Users/Ben/Cloud", true, true},
		{"/mnt/D", true, true},
		{"/mnt/c/Users/Ben/Cloud", false, false},
		{"/mnt/data/backups", true, false},
		{"/home/ben/backups", true, false},
	}
	for _, c := range cases {
		if got := onWSLDrive(c.path, c.wsl); got != c.want {
			t.Errorf("onWSLDrive(%q, %v) = %v, want %v", c.path, c.wsl, got, c.want)
		}
	}
}

func TestWatchDir(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan string, 100)
	done := make(chan error, 1)
	ad, _ := Get("syncthing", time.Minute)
	go func() { done <- ad.(Watcher).Watch(ctx, dir, func(r string) { events <- r }) }()

	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// wait until the watcher is set up: write a probe until it is reported
	deadline := time.Now().Add(5 * time.Second)
	for ready := false; !ready; {
		if time.Now().After(deadline) {
			t.Fatal("watcher did not start")
		}
		write("probe.jwlibrary")
		select {
		case <-events:
			ready = true
		case <-time.After(100 * time.Millisecond):
		}
	}
	drain := func() {
		for {
			select {
			case <-events:
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}
	drain()
	expectNone := func(what string) {
		t.Helper()
		select {
		case r := <-events:
			t.Errorf("%s: unexpected event %q", what, r)
		case <-time.After(300 * time.Millisecond):
		}
	}
	expectOne := func(what string) {
		t.Helper()
		select {
		case <-events:
			drain()
		case <-time.After(3 * time.Second):
			t.Errorf("%s: no event", what)
		}
	}

	write("current.jwlibrary")
	expectNone("current.jwlibrary")
	write(".syncthing.UserdataBackup.jwlibrary.tmp")
	expectNone("syncthing temp file")
	write("notes.txt")
	expectNone("other file")
	os.Mkdir(filepath.Join(dir, "archive"), 0o755)
	write(filepath.Join("archive", "old.jwlibrary"))
	expectNone("file in archive")
	os.Remove(filepath.Join(dir, "probe.jwlibrary"))
	expectNone("removal")
	os.Rename(filepath.Join(dir, "current.jwlibrary"), filepath.Join(dir, "archive", "moved.jwlibrary"))
	expectNone("move out of the folder")

	os.Rename(filepath.Join(dir, ".syncthing.UserdataBackup.jwlibrary.tmp"), filepath.Join(dir, "UserdataBackup_A.jwlibrary"))
	expectOne("rename from temp name")
	write("UserdataBackup_B.jwlibrary")
	expectOne("new backup")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v after cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Watch did not stop after cancel")
	}
}
