package folder

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Samsung_SM-X520": "Samsung_SM-X520",
		"Ben's PC/1":      "Ben_s_PC_1",
		"":                "unknown",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive", ".lock")
	now := time.Now()
	l, err := AcquireLock(path, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path, time.Hour, now); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: %v", err)
	}
	if _, err := AcquireLock(path, time.Hour, now.Add(2*time.Hour)); err != nil {
		t.Errorf("stale lock not taken over: %v", err)
	}
	l.Release()
}
