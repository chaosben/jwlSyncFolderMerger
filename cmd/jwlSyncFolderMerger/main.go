// Command jwlSyncFolderMerger merges the JW Library backups of several devices that are
// collected in one (synchronised) folder into current.jwlibrary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/app"
	"github.com/chaosben/jwlSyncFolderMerger/internal/folder"
	"github.com/chaosben/jwlSyncFolderMerger/internal/selfupdate"
	"github.com/chaosben/jwlSyncFolderMerger/internal/syncadapter"
	"github.com/chaosben/jwlSyncFolderMerger/internal/watch"
)

// Set by the release build.
var version = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitGuard = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		cfg       app.Config
		logFormat string
		verbose   bool
		showVer   bool
		expiry    int
		watchMode bool
		debounce  time.Duration
		poll      time.Duration
		noUpdate  bool
		update    bool
		updateInt time.Duration
	)
	fs := flag.NewFlagSet("jwlSyncFolderMerger", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: jwlSyncFolderMerger [flags] <backup folder>\n       jwlSyncFolderMerger -update\n\nMerges all JW Library backups in the folder into current.jwlibrary.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "only report what would happen, change nothing")
	fs.IntVar(&cfg.KeepGenerations, "keep-generations", 5, "number of previous current files kept in archive/generations")
	fs.DurationVar(&cfg.MinAge, "min-age", 2*time.Minute, "ignore backups modified more recently than this (still syncing)")
	fs.Float64Var(&cfg.MaxDeleteRatio, "max-delete-ratio", 0.2, "abort if a run would delete more than this share of highlights or notes")
	fs.IntVar(&expiry, "device-expiry-days", 180, "forget devices without a new backup for this many days")
	fs.StringVar(&cfg.Sync, "sync", "generic", "sync tool conventions: "+strings.Join(syncadapter.Names(), ", "))
	fs.BoolVar(&cfg.NoMarker, "no-marker", false, "do not embed the generation marker (disables restore detection)")
	fs.BoolVar(&watchMode, "watch", false, "keep running and merge whenever new backups arrive")
	fs.DurationVar(&debounce, "debounce", 15*time.Second, "watch mode: quiet time after the last change before merging")
	fs.DurationVar(&poll, "poll", 0, "watch mode: polling interval (default 6h with change events, 5m without)")
	fs.BoolVar(&noUpdate, "no-auto-update", false, "watch mode: do not install new releases automatically")
	fs.DurationVar(&updateInt, "update-interval", 24*time.Hour, "watch mode: how often to check for a new release (at least 1h)")
	fs.BoolVar(&update, "update", false, "install the latest release and exit (with -dry-run: only check)")
	fs.StringVar(&logFormat, "log-format", "text", "log format: text or json")
	fs.BoolVar(&verbose, "v", false, "verbose logging (every conflict decision)")
	fs.BoolVar(&showVer, "version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return exitError
	}
	if showVer {
		fmt.Println("jwlSyncFolderMerger", version)
		return exitOK
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if logFormat == "json" {
		cfg.Log = slog.New(slog.NewJSONHandler(os.Stderr, opts))
	} else {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, opts))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if update {
		return selfUpdate(ctx, cfg.Log, cfg.DryRun)
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitError
	}
	cfg.Dir = fs.Arg(0)
	cfg.DeviceExpiry = time.Duration(expiry) * 24 * time.Hour
	if err := selfupdate.ValidateInterval(updateInt); err != nil {
		cfg.Log.Error(err.Error())
		return exitError
	}

	if watchMode {
		// a missing folder at start is a configuration error; later outages are retried
		if info, err := os.Stat(cfg.Dir); err != nil || !info.IsDir() {
			cfg.Log.Error("backup folder not accessible", "folder", cfg.Dir)
			return exitError
		}
		ad, err := syncadapter.Get(cfg.Sync, cfg.MinAge)
		if err != nil {
			cfg.Log.Error(err.Error())
			return exitError
		}
		cfg.Log.Info("watching", "folder", cfg.Dir, "sync", cfg.Sync, "version", version)
		wcfg := watch.Config{
			Run:          func(ctx context.Context) (*app.Report, error) { return app.Run(ctx, cfg) },
			Adapter:      ad,
			Dir:          cfg.Dir,
			Debounce:     debounce,
			Poll:         poll,
			FallbackPoll: poll,
			PendingRetry: cfg.MinAge,
			Log:          cfg.Log,
		}
		var up *selfupdate.Updater
		switch {
		case noUpdate:
			cfg.Log.Info("automatic updates disabled", "reason", "-no-auto-update")
		case cfg.DryRun:
			cfg.Log.Info("automatic updates disabled", "reason", "-dry-run")
		default:
			if up = updater(cfg.Log); up != nil {
				wcfg.Update, wcfg.UpdateEvery = up.Update, updateInt
			}
		}
		err = watch.Loop(ctx, wcfg)
		if errors.Is(err, watch.ErrRestart) {
			code, err := up.Restart()
			if err != nil {
				cfg.Log.Error(err.Error())
				return exitError
			}
			return code
		}
		if err != nil {
			cfg.Log.Error(err.Error())
			return exitError
		}
		cfg.Log.Info("stopped")
		return exitOK
	}

	rep, err := app.Run(ctx, cfg)
	switch {
	case errors.Is(err, folder.ErrLocked):
		cfg.Log.Info(err.Error())
		return exitOK
	case errors.Is(err, app.ErrGuard):
		cfg.Log.Error(err.Error())
		return exitGuard
	case err != nil:
		cfg.Log.Error(err.Error())
		return exitError
	}
	s := rep.Stats
	cfg.Log.Info("done", "outcome", rep.Outcome, "devices", strings.Join(rep.Devices, ","),
		"changed_units", s.Changed, "conflicts", s.Conflicts, "deleted_units", s.Deleted,
		"removed_highlights", rep.Loss.Marks, "removed_notes", rep.Loss.Notes,
		"divergent_paragraph_groups", s.DivergentGroup, "moved_bookmarks", s.MovedBookmarks, "lost_bookmarks", s.LostBookmarks,
		"dry_run", cfg.DryRun)
	return exitOK
}

// updater returns the self-updater for watch mode, or nil if this
// installation cannot update itself.
func updater(log *slog.Logger) *selfupdate.Updater {
	up, err := selfupdate.New(version, log)
	if err == nil {
		err = up.Supported()
	}
	if err != nil {
		log.Info("automatic updates disabled", "reason", err)
		return nil
	}
	up.CleanupOld()
	return up
}

// selfUpdate implements -update.
func selfUpdate(ctx context.Context, log *slog.Logger, dryRun bool) int {
	up, err := selfupdate.New(version, log)
	if err == nil {
		err = up.Supported()
	}
	if err != nil {
		log.Error("cannot update", "err", err)
		return exitError
	}
	up.CleanupOld()
	if dryRun {
		rel, err := up.Check(ctx)
		switch {
		case err != nil:
			log.Error(err.Error())
			return exitError
		case rel != nil:
			log.Info("dry-run: would update", "version", version, "new_version", rel.Version)
		default:
			log.Info("up to date", "version", version)
		}
		return exitOK
	}
	updated, err := up.Update(ctx)
	if err != nil {
		log.Error(err.Error())
		return exitError
	}
	if !updated {
		log.Info("up to date", "version", version)
	}
	return exitOK
}
