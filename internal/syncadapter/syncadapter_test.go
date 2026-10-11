package syncadapter

import (
	"io/fs"
	"testing"
	"time"
)

type fakeInfo struct {
	fs.FileInfo
	mod time.Time
}

func (f fakeInfo) ModTime() time.Time { return f.mod }

func TestSyncthingIgnore(t *testing.T) {
	ad, err := Get("syncthing", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"UserdataBackup_2026-10-11_X.jwlibrary":                                       false,
		"UserdataBackup_2026-10-11_X.sync-conflict-20260108-151649-6RQTHMM.jwlibrary": false,
		".syncthing.UserdataBackup.jwlibrary.tmp":                                     true,
		"~syncthing~UserdataBackup.jwlibrary.tmp":                                     true,
		"UserdataBackup.jwlibrary.part":                                               true,
	} {
		if got := ad.Ignore(name); got != want {
			t.Errorf("Ignore(%q) = %v, want %v", name, got, want)
		}
	}
	now := time.Now()
	if ad.Stable(fakeInfo{mod: now.Add(-30 * time.Second)}, now) {
		t.Error("fresh file must not be stable")
	}
	if !ad.Stable(fakeInfo{mod: now.Add(-2 * time.Minute)}, now) {
		t.Error("old file must be stable")
	}
	if _, err := Get("nope", 0); err == nil {
		t.Error("unknown adapter accepted")
	}
}
