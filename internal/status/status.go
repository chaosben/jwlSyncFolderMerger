// Package status maintains jwlsfm_status.html in the backup folder: a
// self-contained page with the current state at the top and a log of the most
// recent relevant events below. The page carries its own data as JSON, so
// separate processes (watch mode, one-off runs) continue the same log.
package status

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Kinds of log entries.
const (
	KindMerged    = "merged"
	KindUnchanged = "unchanged"
	KindGuard     = "guard"
	KindError     = "error"
	KindWarning   = "warning"
	KindUpdate    = "update"
	KindStart     = "start"
	KindStop      = "stop"
)

// Entry is one line of the log.
type Entry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// Device is the state of one known device.
type Device struct {
	Name       string    `json:"name"`
	LastBackup time.Time `json:"last_backup"`
	// Restored: "current" (its last backup contains the current generation),
	// "older" (an older generation), "never" or "" (unknown, no markers).
	Restored string `json:"restored"`
}

// Current describes current.jwlibrary.
type Current struct {
	Time          time.Time `json:"time"`
	Generation    string    `json:"generation"`
	Highlights    int       `json:"highlights"`
	Notes         int       `json:"notes"`
	Tags          int       `json:"tags"`
	Bookmarks     int       `json:"bookmarks"`
	PlaylistItems int       `json:"playlist_items"`
}

// Run is the result of one merge run.
type Run struct {
	Time     time.Time `json:"time"`
	Outcome  string    `json:"outcome"` // merged, unchanged, nothing-to-do, guard, error
	Error    string    `json:"error,omitempty"`
	Pending  int       `json:"pending,omitempty"`
	Warnings int       `json:"warnings,omitempty"`
	Current  *Current  `json:"-"` // nil: unknown, keep the previous state
	Devices  []Device  `json:"-"` // nil: unknown, keep the previous state
}

// Mode describes how the tool is running.
type Mode struct {
	Watch      bool          `json:"watch"`
	Since      time.Time     `json:"since,omitzero"`
	Stopped    time.Time     `json:"stopped,omitzero"`      // watch mode ended cleanly
	AutoUpdate string        `json:"auto_update,omitempty"` // "on" or "off (reason)"
	Expect     time.Duration `json:"expect,omitempty"`      // maximum time between two writes
}

// state is everything the page shows; it is stored inside the page.
type state struct {
	Format     int       `json:"format"`
	Host       string    `json:"host"`
	Version    string    `json:"version"`
	Mode       Mode      `json:"mode"`
	LastRun    *Run      `json:"last_run,omitempty"`
	LastMerge  time.Time `json:"last_merge,omitzero"`
	Current    *Current  `json:"current,omitempty"`
	Devices    []Device  `json:"devices,omitempty"`
	Written    time.Time `json:"written"`
	StaleAfter time.Time `json:"stale_after,omitzero"`
	Entries    []Entry   `json:"entries"` // newest first
}

const format = 1

// Writer updates the status page. A nil Writer does nothing.
type Writer struct {
	path    string
	tmpDir  string
	keep    int
	version string
	log     *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	mode Mode
}

// New returns a writer for <dir>/name that keeps the last keep entries and
// writes temporary files to tmpDir; keep <= 0 disables the page (nil).
func New(dir, name, tmpDir string, keep int, version string, log *slog.Logger) *Writer {
	if keep <= 0 {
		return nil
	}
	return &Writer{
		path: filepath.Join(dir, name), tmpDir: tmpDir, keep: keep,
		version: version, log: log, now: time.Now,
	}
}

// SetMode records how the tool runs; it is shown with every later write.
func (w *Writer) SetMode(m Mode) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mode = m
}

// Run records a merge run and the log entries it produced.
func (w *Writer) Run(r Run, entries ...Entry) {
	if w == nil {
		return
	}
	w.update(func(s *state) {
		run := r
		s.LastRun = &run
		if r.Outcome == KindMerged {
			s.LastMerge = r.Time
		}
		if r.Current != nil {
			s.Current = r.Current
		}
		if r.Devices != nil {
			s.Devices = r.Devices
		}
		s.add(entries...)
	})
}

// Event adds a log entry that is not tied to a merge run.
func (w *Writer) Event(e Entry) {
	if w == nil {
		return
	}
	w.update(func(s *state) { s.add(e) })
}

// repeatAfter suppresses identical problem entries (e.g. a warning that every
// poll reports again) so that they do not push everything else out of the log.
const repeatAfter = 24 * time.Hour

func (s *state) add(entries ...Entry) {
	for _, e := range entries {
		if s.recent(e) {
			continue
		}
		s.Entries = append([]Entry{e}, s.Entries...)
	}
}

func (s *state) recent(e Entry) bool {
	if e.Kind != KindWarning && e.Kind != KindError && e.Kind != KindGuard {
		return false
	}
	for _, o := range s.Entries {
		if o.Kind == e.Kind && o.Text == e.Text && e.Time.Sub(o.Time) < repeatAfter {
			return true
		}
	}
	return false
}

func (w *Writer) update(change func(*state)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.load()
	change(s)
	now := w.now()
	if len(s.Entries) > w.keep {
		s.Entries = s.Entries[:w.keep]
	}
	s.Format = format
	s.Host, _ = os.Hostname()
	s.Version = w.version
	s.Mode = w.mode
	s.Written = now
	s.StaleAfter = time.Time{}
	if w.mode.Watch && w.mode.Stopped.IsZero() && w.mode.Expect > 0 {
		s.StaleAfter = now.Add(w.mode.Expect)
	}
	if err := w.write(s); err != nil {
		w.log.Warn("cannot write status page", "file", w.path, "err", err)
	}
}

var dataRE = regexp.MustCompile(`(?s)<script type="application/json" id="jwlsfm-status">(.*?)</script>`)

// load reads the state from the existing page; anything unreadable starts over.
func (w *Writer) load() *state {
	s := &state{}
	raw, err := os.ReadFile(w.path)
	if err != nil {
		return s
	}
	m := dataRE.FindSubmatch(raw)
	if m == nil || json.Unmarshal(m[1], s) != nil || s.Format != format {
		w.log.Info("status page not readable, starting a new one", "file", w.path)
		return &state{}
	}
	return s
}

// write renders the page and moves it into place atomically.
func (w *Writer) write(s *state) error {
	data, err := json.Marshal(s) // escapes <, > and & – safe inside <script>
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, view{state: s, Data: template.JS(data)}); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := os.MkdirAll(w.tmpDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(w.tmpDir, "status-*.html")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), w.path); err != nil {
		return err
	}
	os.Remove(w.tmpDir) // only if empty; the merge uses it as well
	return nil
}
