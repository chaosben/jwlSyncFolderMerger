package app

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/model"
	"github.com/chaosben/jwlSyncFolderMerger/internal/status"
)

func currentInfo(db *model.DB, t time.Time) *status.Current {
	return &status.Current{
		Time: t, Generation: db.Marker,
		Highlights: len(db.UserMarks), Notes: len(db.Notes), Tags: len(db.Tags),
		Bookmarks: len(db.Bookmarks), PlaylistItems: len(db.Playlists),
	}
}

// summarize records the state of current and of the known devices after a
// successful run. Expired devices are left out.
func (r *runner) summarize(devices map[string]*device, names []string, cur *status.Current) {
	r.rep.Current = cur
	r.rep.Known = []status.Device{}
	for _, n := range names {
		d := devices[n]
		latest := d.base
		if len(d.incoming) > 0 {
			latest = d.incoming[len(d.incoming)-1]
		}
		if latest == nil || (len(d.incoming) == 0 && r.now.Sub(latest.snap.Time) > r.cfg.DeviceExpiry) {
			continue
		}
		dev := status.Device{Name: n, LastBackup: latest.snap.Time}
		switch m := latest.snap.DB.Marker; {
		case cur == nil || cur.Generation == "":
		case m == cur.Generation:
			dev.Restored = "current"
		case m != "":
			dev.Restored = "older"
		default:
			dev.Restored = "never"
		}
		r.rep.Known = append(r.rep.Known, dev)
	}
}

// statusRun turns the result of a run into the status page update and its log
// entries; runs without new backups only update the head.
func statusRun(now time.Time, rep *Report, err error) (status.Run, []status.Entry) {
	run := status.Run{
		Time: now, Outcome: string(rep.Outcome), Pending: rep.Pending,
		Warnings: len(rep.Warnings), Current: rep.Current, Devices: rep.Known,
	}
	var entries []status.Entry
	add := func(kind, text string) {
		entries = append(entries, status.Entry{Time: now, Kind: kind, Text: text})
	}
	for _, w := range rep.Warnings {
		add(status.KindWarning, w)
	}
	switch {
	case errors.Is(err, ErrGuard):
		run.Outcome, run.Error = status.KindGuard, err.Error()
		add(status.KindGuard, "Aborted, nothing changed: "+err.Error())
	case err != nil:
		run.Outcome, run.Error = status.KindError, err.Error()
		add(status.KindError, "Run failed: "+err.Error())
	case rep.Outcome == Merged:
		s, l := rep.Stats, rep.Loss
		var details []string
		for _, c := range []struct {
			n    int
			what string
		}{{s.Changed, "changed"}, {s.Conflicts, "conflicts"}, {s.Deleted, "deleted"},
			{l.Marks, "highlights removed"}, {l.Notes, "notes removed"}} {
			if c.n > 0 {
				details = append(details, fmt.Sprintf("%d %s", c.n, c.what))
			}
		}
		text := "Merged new backups of " + strings.Join(rep.Devices, ", ")
		if len(details) > 0 {
			text += ": " + strings.Join(details, ", ")
		}
		if rep.Current != nil && rep.Current.Generation != "" {
			text += " – generation " + rep.Current.Generation
		}
		add(status.KindMerged, text)
	case rep.Outcome == Unchanged:
		add(status.KindUnchanged, "Processed new backups of "+strings.Join(rep.Devices, ", ")+
			": nothing new, current.jwlibrary stays as it is")
	}
	return run, entries
}
