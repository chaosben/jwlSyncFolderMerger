// Package syncadapter encapsulates what differs between file sync tools:
// which files are their temporary/internal files and when a file can be
// considered completely synchronised. New tools get their own file here.
package syncadapter

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// Adapter decides which files in the backup folder the merger may touch.
type Adapter interface {
	Name() string
	// Ignore reports whether a file (base name) belongs to the sync tool
	// and must neither be read nor deleted.
	Ignore(name string) bool
	// Stable reports whether a file is completely synchronised.
	Stable(info fs.FileInfo, now time.Time) bool
}

var registry = map[string]func(minAge time.Duration) Adapter{
	"generic":   func(d time.Duration) Adapter { return generic{minAge: d} },
	"syncthing": func(d time.Duration) Adapter { return syncthing{generic{minAge: d}} },
}

// Names lists the available adapters.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get returns the adapter with the given name.
func Get(name string, minAge time.Duration) (Adapter, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown sync adapter %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f(minAge), nil
}

// generic covers the usual temporary-file conventions and waits until a file
// has not been modified for minAge.
type generic struct{ minAge time.Duration }

func (generic) Name() string { return "generic" }

func (generic) Ignore(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") ||
		strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".partial")
}

func (g generic) Stable(info fs.FileInfo, now time.Time) bool {
	return now.Sub(info.ModTime()) >= g.minAge
}

// syncthing additionally knows Syncthing's temp files. Its conflict copies
// (*.sync-conflict-*) are real backups and stay inputs; duplicates are
// removed by content hash.
type syncthing struct{ generic }

func (syncthing) Name() string { return "syncthing" }

func (s syncthing) Ignore(name string) bool {
	return s.generic.Ignore(name) || strings.HasPrefix(name, ".syncthing.") || strings.HasPrefix(name, "~syncthing~")
}
