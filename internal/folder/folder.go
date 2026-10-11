// Package folder knows the layout of the backup folder:
//
//	<root>/current.jwlibrary                      merged result, restored on every device
//	<root>/*.jwlibrary                            incoming device backups
//	<root>/archive/generations/current_*.jwlibrary previous merged results
//	<root>/archive/devices/<device>.jwlibrary     last processed backup per device
package folder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/syncadapter"
)

const (
	CurrentName = "current.jwlibrary"
	Ext         = ".jwlibrary"
)

type Layout struct{ Root string }

func (l Layout) Current() string     { return filepath.Join(l.Root, CurrentName) }
func (l Layout) Archive() string     { return filepath.Join(l.Root, "archive") }
func (l Layout) Generations() string { return filepath.Join(l.Archive(), "generations") }
func (l Layout) Devices() string     { return filepath.Join(l.Archive(), "devices") }
func (l Layout) TempDir() string     { return filepath.Join(l.Archive(), ".tmp") }
func (l Layout) LockFile() string    { return filepath.Join(l.Archive(), ".jwlSyncFolderMerger.lock") }

// DeviceFile is where the last processed backup of a device is kept.
func (l Layout) DeviceFile(device string) string {
	return filepath.Join(l.Devices(), SafeName(device)+Ext)
}

// GenerationFile is where a previous current is archived.
func (l Layout) GenerationFile(t time.Time) string {
	return filepath.Join(l.Generations(), "current_"+t.UTC().Format("20060102T150405Z")+Ext)
}

// SafeName makes a device name usable as a file name on all platforms.
func SafeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	return sb.String()
}

// Skipped is an incoming file that is left alone in this run.
type Skipped struct {
	Path    string
	Reason  string
	Syncing bool // still being written by the sync tool; retry later
}

// Incoming lists the device backups in the root folder that are ready to be
// processed. Files the sync tool still works on are skipped.
func (l Layout) Incoming(ad syncadapter.Adapter, now time.Time) (ready []string, skipped []Skipped, err error) {
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), Ext) || strings.EqualFold(name, CurrentName) {
			continue
		}
		path := filepath.Join(l.Root, name)
		if ad.Ignore(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			skipped = append(skipped, Skipped{Path: path, Reason: err.Error()})
			continue
		}
		if !ad.Stable(info, now) {
			skipped = append(skipped, Skipped{Path: path, Reason: "recently modified, possibly still syncing", Syncing: true})
			continue
		}
		ready = append(ready, path)
	}
	sort.Strings(ready)
	return ready, skipped, nil
}

// List returns the .jwlibrary files of a directory (missing directory = none).
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), Ext) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Lock is an exclusive lock file; a lock older than staleAfter is taken over.
type Lock struct{ path string }

var ErrLocked = errors.New("another jwlSyncFolderMerger run is in progress")

func AcquireLock(path string, staleAfter time.Duration, now time.Time) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d\nstarted %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
			f.Close()
			return &Lock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, serr := os.Stat(path)
		if serr != nil || now.Sub(info.ModTime()) < staleAfter {
			return nil, ErrLocked
		}
		os.Remove(path) // stale
	}
	return nil, ErrLocked
}

func (l *Lock) Release() error { return os.Remove(l.path) }

// UniquePath returns path, or path with a numeric suffix if it exists.
func UniquePath(path string) string {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return path
	}
	base := strings.TrimSuffix(path, Ext)
	for i := 2; ; i++ {
		p := base + "_" + strconv.Itoa(i) + Ext
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}
