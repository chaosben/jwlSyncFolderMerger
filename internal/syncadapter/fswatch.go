package syncadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// Watcher is an optional capability: the adapter can notify about changes in
// the backup folder instead of being polled.
type Watcher interface {
	// Watch blocks until ctx is done and calls changed for every relevant
	// change. It returns ErrNoEvents if the folder cannot be watched at all.
	Watch(ctx context.Context, dir string, changed func(reason string)) error
}

// ErrNoEvents means the filesystem does not deliver change events for the
// folder (e.g. a Windows drive mounted into WSL); callers fall back to polling.
var ErrNoEvents = errors.New("filesystem change events are not available for this folder")

func (g generic) Watch(ctx context.Context, dir string, changed func(string)) error {
	return watchDir(ctx, dir, g.Ignore, changed)
}

func (s syncthing) Watch(ctx context.Context, dir string, changed func(string)) error {
	return watchDir(ctx, dir, s.Ignore, changed)
}

// watchDir watches the backup folder (not recursively). A new or rewritten
// backup in the folder itself is relevant; removals and the old name of a
// rename are not – that is what our own cleanup produces. The new name of a
// rename arrives as a Create event, which covers sync tools that write to a
// temporary name first.
func watchDir(ctx context.Context, dir string, ignore func(string) bool, changed func(string)) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if onWSLDrive(abs, isWSL()) {
		return ErrNoEvents
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoEvents, err)
	}
	defer w.Close()
	if err := w.Add(abs); err != nil {
		return fmt.Errorf("watch %s: %w", abs, err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("filesystem watcher closed")
			}
			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
				if relevant(abs, ev.Name, ignore) {
					changed(strings.ToLower(ev.Op.String()) + " " + filepath.Base(ev.Name))
				}
			}
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("filesystem watcher closed")
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				changed("resync")
				continue
			}
			return fmt.Errorf("filesystem watcher: %w", err)
		}
	}
}

func relevant(dir, path string, ignore func(string) bool) bool {
	if filepath.Dir(path) != dir {
		return false
	}
	name := filepath.Base(path)
	return strings.EqualFold(filepath.Ext(name), ".jwlibrary") &&
		!strings.EqualFold(name, "current.jwlibrary") && !ignore(name)
}

var wslDrive = regexp.MustCompile(`^/mnt/[a-zA-Z](/|$)`)

// onWSLDrive reports whether path is a Windows drive mounted into WSL, where
// changes made on the Windows side produce no inotify events.
func onWSLDrive(path string, wsl bool) bool {
	return wsl && wslDrive.MatchString(filepath.ToSlash(path))
}

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(raw)), "microsoft")
}
