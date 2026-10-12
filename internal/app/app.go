// Package app runs one merge cycle over a backup folder:
// lock → scan → pick bases → merge → safety checks → write current → tidy up.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/dbwrite"
	"github.com/chaosben/jwlSyncFolderMerger/internal/folder"
	"github.com/chaosben/jwlSyncFolderMerger/internal/jwlfile"
	"github.com/chaosben/jwlSyncFolderMerger/internal/jwltime"
	"github.com/chaosben/jwlSyncFolderMerger/internal/merge"
	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
	"github.com/chaosben/jwlSyncFolderMerger/internal/status"
	"github.com/chaosben/jwlSyncFolderMerger/internal/syncadapter"
)

// DeviceName is written into the manifest of every merged current.
const DeviceName = "jwlSyncFolderMerger"

// ErrGuard means the run was aborted by a safety check; nothing was changed.
var ErrGuard = errors.New("aborted by safety check")

type Config struct {
	Dir             string
	DryRun          bool
	KeepGenerations int
	MinAge          time.Duration
	MaxDeleteRatio  float64
	DeviceExpiry    time.Duration
	Sync            string
	NoMarker        bool
	Log             *slog.Logger
	Now             func() time.Time
	WorkDir         string         // parent for temporary extraction, "" = system temp
	Status          *status.Writer // status page, nil = none; never written in dry-run mode
}

type Outcome string

const (
	NothingToDo Outcome = "nothing-to-do"
	Unchanged   Outcome = "unchanged" // backups processed, merged content equals current
	Merged      Outcome = "merged"
)

type Report struct {
	Outcome  Outcome
	Pending  int // backups skipped because they may still be syncing
	Devices  []string
	Stats    merge.Stats
	Loss     merge.Loss
	Actions  []string
	Warnings []string
	Current  *status.Current // current.jwlibrary after the run; nil if the run failed
	Known    []status.Device // known devices after the run; nil if the run failed
}

type loaded struct {
	file *jwlfile.File
	snap *merge.Snapshot
}

type runner struct {
	cfg   Config
	lay   folder.Layout
	ad    syncadapter.Adapter
	now   time.Time
	work  string
	log   *slog.Logger
	rep   *Report
	media map[string]string // media hash -> verified file on disk
	thumb string
	files map[*merge.Snapshot]*jwlfile.File
	ops   []op

	newCurrent *model.DB // content of the current written by this run
}

type op struct {
	kind     string // "copy", "rename" or "delete"
	from, to string
}

func (c *Config) defaults() {
	if c.KeepGenerations <= 0 {
		c.KeepGenerations = 5
	}
	if c.MaxDeleteRatio <= 0 {
		c.MaxDeleteRatio = 0.2
	}
	if c.DeviceExpiry <= 0 {
		c.DeviceExpiry = 180 * 24 * time.Hour
	}
	if c.Sync == "" {
		c.Sync = "generic"
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Run performs one merge cycle.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg.defaults()
	ad, err := syncadapter.Get(cfg.Sync, cfg.MinAge)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(cfg.Dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("backup folder %q not accessible", cfg.Dir)
	}
	r := &runner{
		cfg: cfg, lay: folder.Layout{Root: cfg.Dir}, ad: ad, now: cfg.Now().UTC(),
		log: cfg.Log, rep: &Report{}, media: map[string]string{},
		files: map[*merge.Snapshot]*jwlfile.File{},
	}
	if !cfg.DryRun {
		lock, err := folder.AcquireLock(r.lay.LockFile(), time.Hour, r.now)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
	}
	if r.work, err = os.MkdirTemp(cfg.WorkDir, "jwlSyncFolderMerger-"); err != nil {
		return nil, err
	}
	defer os.RemoveAll(r.work)
	err = r.run()
	if !cfg.DryRun {
		// still under the lock, so parallel processes do not interleave
		run, entries := statusRun(r.now, r.rep, err)
		cfg.Status.Run(run, entries...)
	}
	return r.rep, err
}

func (r *runner) warn(msg string, args ...any) {
	r.log.Warn(msg, args...)
	var kv []string
	for i := 0; i+1 < len(args); i += 2 {
		kv = append(kv, fmt.Sprintf("%v: %v", args[i], args[i+1]))
	}
	if len(kv) > 0 {
		msg += " (" + strings.Join(kv, ", ") + ")"
	}
	r.rep.Warnings = append(r.rep.Warnings, msg)
}

func (r *runner) load(path string) (*loaded, error) {
	f, err := jwlfile.Open(path, r.work)
	if err != nil {
		return nil, err
	}
	conn, err := jwlfile.OpenDB(f.DBPath)
	if err != nil {
		return nil, err
	}
	db, err := model.Load(conn)
	conn.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(db.Warnings) > 0 {
		r.log.Debug("dangling references dropped", "file", filepath.Base(path), "count", len(db.Warnings))
	}
	for h, m := range db.Media {
		if _, ok := r.media[h]; ok {
			continue
		}
		if p, ok := f.Files[m.FilePath]; ok {
			if got, err := jwlfile.HashFile(p); err == nil && got == h {
				r.media[h] = p
			}
		}
	}
	if p, ok := f.Files[jwlfile.ThumbnailName]; ok && r.thumb == "" {
		r.thumb = p
	}
	snap := &merge.Snapshot{Name: filepath.Base(path), Time: f.Modified, DB: db}
	r.files[snap] = f
	return &loaded{file: f, snap: snap}, nil
}

type device struct {
	name     string
	base     *loaded   // last processed backup (archive/devices)
	extras   []string  // further archived files of this device
	incoming []*loaded // new backups, oldest first
}

func (r *runner) run() error {
	// previous result and its generations
	var current *loaded
	if _, err := os.Stat(r.lay.Current()); err == nil {
		if current, err = r.load(r.lay.Current()); err != nil {
			return fmt.Errorf("current.jwlibrary unreadable, refusing to continue: %w", err)
		}
	}
	gens := map[string]*merge.Snapshot{}
	if current != nil && current.snap.DB.Marker != "" {
		gens[current.snap.DB.Marker] = current.snap
	}
	genFiles, err := folder.List(r.lay.Generations())
	if err != nil {
		return err
	}
	for _, p := range genFiles {
		g, err := r.load(p)
		if err != nil {
			r.warn("archived generation unreadable", "file", p, "err", err)
			continue
		}
		if g.snap.DB.Marker != "" {
			gens[g.snap.DB.Marker] = g.snap
		}
	}

	// devices: archived bases and incoming backups
	devices := map[string]*device{}
	dev := func(name string) *device {
		if devices[name] == nil {
			devices[name] = &device{name: name}
		}
		return devices[name]
	}
	baseFiles, err := folder.List(r.lay.Devices())
	if err != nil {
		return err
	}
	for _, p := range baseFiles {
		b, err := r.load(p)
		if err != nil {
			r.warn("archived device backup unreadable", "file", p, "err", err)
			continue
		}
		d := dev(b.file.Device())
		if d.base == nil || b.snap.Time.After(d.base.snap.Time) {
			if d.base != nil {
				d.extras = append(d.extras, d.base.file.Path)
			}
			d.base = b
		} else {
			d.extras = append(d.extras, p)
		}
	}
	ready, skipped, err := r.lay.Incoming(r.ad, r.now)
	if err != nil {
		return err
	}
	for _, s := range skipped {
		r.log.Info("skipping file", "file", filepath.Base(s.Path), "reason", s.Reason)
		if s.Syncing {
			r.rep.Pending++
		}
	}
	seen := map[string]string{} // db hash -> file
	for _, d := range devices {
		if d.base != nil {
			seen[d.base.file.DBHash] = d.base.file.Path
		}
	}
	for _, p := range ready {
		in, err := r.load(p)
		if err != nil {
			r.warn("backup unreadable, left untouched", "file", filepath.Base(p), "err", err)
			continue
		}
		name := in.file.Device()
		switch {
		case name == "" || name == DeviceName:
			r.warn("not a device backup, left untouched", "file", filepath.Base(p), "device", name)
			continue
		case seen[in.file.DBHash] != "":
			r.log.Info("duplicate backup", "file", filepath.Base(p), "same_as", filepath.Base(seen[in.file.DBHash]))
			r.plan("delete", p, "")
			continue
		}
		seen[in.file.DBHash] = p
		d := dev(name)
		if d.base != nil && !in.snap.Time.After(d.base.snap.Time) {
			r.log.Info("backup older than the processed one", "file", filepath.Base(p), "device", name)
			r.plan("delete", p, "")
			continue
		}
		d.incoming = append(d.incoming, in)
	}

	names := make([]string, 0, len(devices))
	for n := range devices {
		names = append(names, n)
	}
	sort.Strings(names)
	var sources []*merge.Source
	for _, n := range names {
		d := devices[n]
		for _, p := range d.extras {
			r.plan("delete", p, "")
		}
		sort.SliceStable(d.incoming, func(i, j int) bool {
			if !d.incoming[i].snap.Time.Equal(d.incoming[j].snap.Time) {
				return d.incoming[i].snap.Time.Before(d.incoming[j].snap.Time)
			}
			return d.incoming[i].snap.Name < d.incoming[j].snap.Name
		})
		if len(d.incoming) == 0 {
			if d.base != nil && r.now.Sub(d.base.snap.Time) > r.cfg.DeviceExpiry {
				r.log.Info("device expired", "device", n, "last_backup", d.base.snap.Time)
				r.plan("delete", d.base.file.Path, "")
				continue
			}
			if current == nil && d.base != nil {
				// no current to reflect this device: use its last backup as input
				sources = append(sources, &merge.Source{Device: n, Chain: []*merge.Snapshot{d.base.snap}})
			}
			continue
		}
		src := r.source(d, gens)
		sources = append(sources, src)
		r.rep.Devices = append(r.rep.Devices, n)
		last := d.incoming[len(d.incoming)-1]
		r.plan("rename", last.file.Path, r.lay.DeviceFile(n))
		for _, in := range d.incoming[:len(d.incoming)-1] {
			r.plan("delete", in.file.Path, "")
		}
	}

	var curInfo *status.Current
	if current != nil {
		curInfo = currentInfo(current.snap.DB, current.snap.Time)
	}
	if len(r.rep.Devices) == 0 && current != nil {
		r.rep.Outcome = NothingToDo
		r.log.Info("no new device backups")
		r.summarize(devices, names, curInfo)
		return r.apply()
	}
	if len(sources) == 0 {
		r.rep.Outcome = NothingToDo
		r.log.Info("no device backups found")
		r.summarize(devices, names, curInfo)
		return r.apply()
	}

	var cur *merge.Snapshot
	if current != nil {
		cur = current.snap
	}
	res, err := merge.Merge(merge.Input{Current: cur, Sources: sources})
	if err != nil {
		return err
	}
	r.rep.Stats = res.Stats
	for _, d := range res.Decisions {
		r.log.Debug("conflict", "unit", d.Unit.String(), "winner", d.Winner, "others", d.Others)
	}

	// safety: refuse mass deletions
	ref := unionLatest(sources)
	if current != nil {
		ref = current.snap.DB
	}
	r.rep.Loss = merge.Removed(ref, res.DB)
	l := r.rep.Loss
	if (l.MarkRatio > r.cfg.MaxDeleteRatio && l.Marks > 20) || (l.NoteRatio > r.cfg.MaxDeleteRatio && l.Notes > 20) {
		return fmt.Errorf("%w: run would remove %d of %d highlights and %d of %d notes (limit %.0f%%)",
			ErrGuard, l.Marks, l.RefMarks, l.Notes, l.RefNotes, r.cfg.MaxDeleteRatio*100)
	}

	if current != nil && merge.SameContent(current.snap.DB, res.DB) {
		r.rep.Outcome = Unchanged
		r.log.Info("merged content equals current, keeping it")
		r.summarize(devices, names, curInfo)
		return r.apply()
	}

	if err := r.writeCurrent(res.DB, sources, current); err != nil {
		return err
	}
	r.rep.Outcome = Merged
	r.summarize(devices, names, currentInfo(r.newCurrent, r.now))
	return r.apply()
}

// source builds the merge history of one device. If a backup carries the
// marker of a known generation that its predecessor did not have, the device
// restored that generation: it becomes the base.
func (r *runner) source(d *device, gens map[string]*merge.Snapshot) *merge.Source {
	src := &merge.Source{Device: d.name}
	prevMarker := ""
	if d.base != nil {
		src.Base = d.base.snap
		prevMarker = d.base.snap.DB.Marker
	}
	chain := make([]*merge.Snapshot, len(d.incoming))
	for i, in := range d.incoming {
		chain[i] = in.snap
	}
	restoreAt := -1
	for k, s := range chain {
		m := s.DB.Marker
		if m != "" && m != prevMarker {
			if _, ok := gens[m]; ok {
				restoreAt = k
			} else {
				r.warn("backup contains an unknown generation (archive too short?)", "device", d.name, "generation", m)
			}
		}
		prevMarker = m
	}
	switch {
	case restoreAt >= 0:
		src.Base = gens[chain[restoreAt].DB.Marker]
		chain = chain[restoreAt:]
	case src.Base == nil && len(chain) > 1:
		// first sight of this device: its oldest backup is the known start
		src.Base = chain[0]
		chain = chain[1:]
	}
	src.Chain = chain
	baseName := "none"
	if src.Base != nil {
		baseName = src.Base.Name
	}
	r.log.Info("device", "name", d.name, "backups", len(chain), "base", baseName, "restored_current", restoreAt >= 0)
	return src
}

func unionLatest(sources []*merge.Source) *model.DB {
	db := model.NewDB()
	for _, s := range sources {
		last := s.Chain[len(s.Chain)-1].DB
		for g, m := range last.UserMarks {
			db.UserMarks[g] = m
		}
		for g, n := range last.Notes {
			db.Notes[g] = n
		}
	}
	return db
}

func newGenerationID(now time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return now.Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

func (r *runner) writeCurrent(db *model.DB, sources []*merge.Source, current *loaded) error {
	// template: newest database with the highest schema version
	var tmpl *merge.Snapshot
	for _, s := range sources {
		for _, c := range s.Chain {
			if tmpl == nil || c.DB.SchemaVersion > tmpl.DB.SchemaVersion ||
				(c.DB.SchemaVersion == tmpl.DB.SchemaVersion && c.Time.After(tmpl.Time)) {
				tmpl = c
			}
		}
	}
	tmplFile := r.fileOf(tmpl)
	if tmplFile == nil {
		return errors.New("internal: template file not found")
	}
	genID := ""
	if !r.cfg.NoMarker {
		genID = newGenerationID(r.now)
	}
	dbPath := filepath.Join(r.work, "merged.db")
	paths, err := dbwrite.Write(tmplFile.DBPath, dbPath, db, dbwrite.Options{Marker: genID, LastModified: r.now})
	if err != nil {
		return fmt.Errorf("write database: %w", err)
	}
	hash, err := jwlfile.HashFile(dbPath)
	if err != nil {
		return err
	}
	files := map[string]string{}
	if r.thumb != "" {
		files[jwlfile.ThumbnailName] = r.thumb
	}
	for h, name := range paths {
		p, ok := r.media[h]
		if !ok {
			return fmt.Errorf("media file for hash %s not found in any backup", h)
		}
		files[name] = p
	}
	m := jwlfile.Manifest{
		Name:         "UserdataBackup_" + r.now.Format("2006-01-02") + "_" + DeviceName + folder.Ext,
		CreationDate: r.now.Format("2006-01-02"),
		Version:      1,
		Type:         0,
		UserDataBackup: jwlfile.UserDataBackup{
			LastModifiedDate: jwltime.FormatDB(r.now),
			DeviceName:       DeviceName,
			DatabaseName:     jwlfile.DefaultDBName,
			Hash:             hash,
			SchemaVersion:    tmplFile.SchemaVersion,
		},
	}
	tmpDir := r.lay.TempDir()
	if r.cfg.DryRun {
		tmpDir = r.work
	} else if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(tmpDir, "current.new"+folder.Ext)
	if err := jwlfile.Write(tmp, m, dbPath, files); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	// verify by reading the archive back
	check, err := r.load(tmp)
	if err != nil {
		return fmt.Errorf("verify new current: %w", err)
	}
	if !merge.SameContent(db, check.snap.DB) || check.snap.DB.Marker != genID || check.file.DBHash != hash {
		return errors.New("verify new current: content differs after writing")
	}
	r.newCurrent = check.snap.DB
	r.log.Info("new current written", "generation", genID, "highlights", len(db.UserMarks), "notes", len(db.Notes),
		"tags", len(db.Tags), "bookmarks", len(db.Bookmarks), "playlist_items", len(db.Playlists))

	// the new current goes first so that later failures never lose data;
	// the old one is copied to the archive and then atomically replaced
	pre := []op{}
	if current != nil {
		pre = append(pre, op{"copy", r.lay.Current(), folder.UniquePath(r.lay.GenerationFile(current.snap.Time))})
	}
	pre = append(pre, op{"rename", tmp, r.lay.Current()})
	r.ops = append(pre, r.ops...)
	return r.pruneGenerations(current != nil)
}

func (r *runner) fileOf(s *merge.Snapshot) *jwlfile.File {
	return r.files[s]
}

func (r *runner) pruneGenerations(adding bool) error {
	files, err := folder.List(r.lay.Generations())
	if err != nil {
		return err
	}
	keep := r.cfg.KeepGenerations
	if adding {
		keep-- // the current being archived now counts as one
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files))) // names carry the timestamp
	for i, p := range files {
		if i >= keep {
			r.plan("delete", p, "")
		}
	}
	return nil
}

func (r *runner) plan(kind, from, to string) {
	r.ops = append(r.ops, op{kind, from, to})
}

// apply performs (or, in dry-run mode, only reports) the planned file
// operations in order.
func (r *runner) apply() error {
	for _, o := range r.ops {
		desc := fmt.Sprintf("%s %s", o.kind, rel(r.lay.Root, o.from))
		if o.to != "" {
			desc += " -> " + rel(r.lay.Root, o.to)
		}
		r.rep.Actions = append(r.rep.Actions, desc)
		if r.cfg.DryRun {
			r.log.Info("dry-run: would " + desc)
			continue
		}
		var err error
		switch o.kind {
		case "rename":
			if err = os.MkdirAll(filepath.Dir(o.to), 0o755); err == nil {
				err = os.Rename(o.from, o.to)
			}
		case "copy":
			if err = os.MkdirAll(filepath.Dir(o.to), 0o755); err == nil {
				err = copyFile(o.from, o.to)
			}
		case "delete":
			err = os.Remove(o.from)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", desc, err)
		}
		r.log.Info(desc)
	}
	if !r.cfg.DryRun {
		os.Remove(r.lay.TempDir())
	}
	return nil
}

func rel(root, p string) string {
	if q, err := filepath.Rel(root, p); err == nil {
		return q
	}
	return p
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
