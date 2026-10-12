// Package watch keeps the merger running: it reacts to change notifications
// of the sync adapter (debounced), falls back to polling, and retries when
// backups are still being synchronised.
package watch

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/app"
	"github.com/chaosben/jwlSyncFolderMerger/internal/folder"
	"github.com/chaosben/jwlSyncFolderMerger/internal/syncadapter"
)

type Config struct {
	Run          func(ctx context.Context) (*app.Report, error)
	Adapter      syncadapter.Adapter
	Dir          string
	Debounce     time.Duration // quiet time after the last change before a run (default 15s)
	Poll         time.Duration // safety-net interval while change events work (default 6h)
	FallbackPoll time.Duration // interval when no change events are available (default 5m)
	PendingRetry time.Duration // retry delay for backups that were still syncing (default 2m)
	Restart      time.Duration // pause before restarting a failed watcher (default 1m)
	Log          *slog.Logger
	// Update, if set, installs a new version and reports whether the process
	// must restart. It runs before the first merge and then every UpdateEvery,
	// never during a merge.
	Update      func(ctx context.Context) (restart bool, err error)
	UpdateEvery time.Duration // default 24h
	UpdateRetry time.Duration // retry after a failed check (default 1h, at most UpdateEvery)
}

// ErrRestart is returned by Loop after Update installed a new version.
var ErrRestart = errors.New("restart after update")

func (c *Config) defaults() {
	if c.Debounce <= 0 {
		c.Debounce = 15 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 6 * time.Hour
	}
	if c.FallbackPoll <= 0 {
		c.FallbackPoll = 5 * time.Minute
	}
	if c.PendingRetry <= 0 {
		c.PendingRetry = 2 * time.Minute
	}
	if c.Restart <= 0 {
		c.Restart = time.Minute
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.UpdateEvery <= 0 {
		c.UpdateEvery = 24 * time.Hour
	}
	if c.UpdateRetry <= 0 {
		c.UpdateRetry = time.Hour
	}
}

const maxBackoff = 30 * time.Minute

// Loop runs until ctx is cancelled or an update requires a restart
// (ErrRestart). It runs once at start, then after every burst of changes, on
// the poll interval and when a retry is due.
func Loop(ctx context.Context, cfg Config) error {
	cfg.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the watcher before a restart
	update := stoppedTimer()
	if cfg.Update != nil {
		next, restart := runUpdate(ctx, cfg)
		if restart {
			return ErrRestart
		}
		if ctx.Err() != nil {
			return nil
		}
		reset(update, next)
	}
	signals := make(chan string, 1)
	notify := func(reason string) {
		select {
		case signals <- reason:
		default: // a run is already scheduled
		}
	}
	var watching atomic.Bool
	lost := make(chan struct{}, 1) // the watcher stopped working
	if w, ok := cfg.Adapter.(syncadapter.Watcher); ok {
		go runWatcher(ctx, w, cfg, &watching, lost, notify)
	} else {
		cfg.Log.Info("sync adapter has no change notifications, polling", "interval", cfg.FallbackPoll)
	}
	interval := func() time.Duration {
		if watching.Load() {
			return cfg.Poll
		}
		return cfg.FallbackPoll
	}

	debounce := stoppedTimer()
	retry := stoppedTimer()
	poll := time.NewTimer(interval())
	pollDue := time.Now().Add(interval())
	backoff := cfg.Debounce
	runNow := true
	for {
		if runNow {
			runNow = false
			next, ok := runOnce(ctx, cfg, &backoff)
			if !ok {
				return nil
			}
			reset(poll, interval())
			pollDue = time.Now().Add(interval())
			if next > 0 {
				reset(retry, next)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-lost:
			// without change events, fall back to the shorter polling interval
			if time.Until(pollDue) > cfg.FallbackPoll {
				reset(poll, cfg.FallbackPoll)
				pollDue = time.Now().Add(cfg.FallbackPoll)
			}
		case reason := <-signals:
			cfg.Log.Debug("change detected", "reason", reason)
			reset(debounce, cfg.Debounce)
		case <-debounce.C:
			runNow = true
		case <-retry.C:
			runNow = true
		case <-poll.C:
			runNow = true
		case <-update.C:
			next, restart := runUpdate(ctx, cfg)
			if restart {
				return ErrRestart
			}
			reset(update, next)
		}
	}
}

// runUpdate checks for a new version and returns the delay until the next check.
func runUpdate(ctx context.Context, cfg Config) (next time.Duration, restart bool) {
	restart, err := cfg.Update(ctx)
	if restart {
		return 0, true
	}
	if err != nil && ctx.Err() == nil {
		retry := min(cfg.UpdateEvery, cfg.UpdateRetry)
		cfg.Log.Warn("update check failed", "err", err, "retry_in", retry)
		return retry, false
	}
	return cfg.UpdateEvery, false
}

// runOnce performs a merge and returns the delay for a retry (0 = none).
// ok is false when the context was cancelled.
func runOnce(ctx context.Context, cfg Config, backoff *time.Duration) (next time.Duration, ok bool) {
	rep, err := cfg.Run(ctx)
	if ctx.Err() != nil {
		return 0, false
	}
	switch {
	case errors.Is(err, folder.ErrLocked):
		cfg.Log.Info("another run is active, retrying", "in", cfg.Debounce)
		return cfg.Debounce, true
	case errors.Is(err, app.ErrGuard):
		// do not retry by itself: only a new backup can change the outcome
		cfg.Log.Error(err.Error())
		return 0, true
	case err != nil:
		cfg.Log.Error("merge failed", "err", err, "retry_in", *backoff)
		next = *backoff
		*backoff = min(*backoff*2, maxBackoff)
		return next, true
	}
	*backoff = cfg.Debounce
	cfg.Log.Info("run finished", "outcome", rep.Outcome, "devices", len(rep.Devices),
		"conflicts", rep.Stats.Conflicts, "removed_highlights", rep.Loss.Marks, "pending", rep.Pending)
	if rep.Pending > 0 {
		return cfg.PendingRetry + time.Second, true
	}
	return 0, true
}

// runWatcher keeps the adapter's watcher alive. While it is down, the loop
// polls at the fallback interval; restarts do not trigger runs themselves,
// so a permanently failing watcher cannot cause a run every minute.
func runWatcher(ctx context.Context, w syncadapter.Watcher, cfg Config, watching *atomic.Bool, lost chan<- struct{}, notify func(string)) {
	for {
		watching.Store(true)
		err := w.Watch(ctx, cfg.Dir, notify)
		watching.Store(false)
		if ctx.Err() != nil {
			return
		}
		select {
		case lost <- struct{}{}:
		default:
		}
		if errors.Is(err, syncadapter.ErrNoEvents) {
			cfg.Log.Warn("no change events for this folder, polling instead", "interval", cfg.FallbackPoll, "err", err)
			return
		}
		cfg.Log.Warn("change watcher stopped, restarting", "err", err, "in", cfg.Restart)
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Restart):
		}
	}
}

func stoppedTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

func reset(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
