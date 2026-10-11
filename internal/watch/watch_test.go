package watch

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/chaosben/jwlSyncFolderMerger/internal/app"
	"github.com/chaosben/jwlSyncFolderMerger/internal/syncadapter"
)

// pollingAdapter has no change notifications.
type pollingAdapter struct{}

func (pollingAdapter) Name() string                       { return "fake" }
func (pollingAdapter) Ignore(string) bool                 { return false }
func (pollingAdapter) Stable(fs.FileInfo, time.Time) bool { return true }

// watchingAdapter forwards notifications from the test.
type watchingAdapter struct {
	pollingAdapter
	notify chan func(string)
	err    error // returned immediately if set
}

func (w watchingAdapter) Watch(ctx context.Context, _ string, changed func(string)) error {
	if w.err != nil {
		return w.err
	}
	w.notify <- changed
	<-ctx.Done()
	return nil
}

type fakeRun struct {
	mu      sync.Mutex
	calls   int
	results []func() (*app.Report, error) // per call; last one repeats
}

func (f *fakeRun) run(context.Context) (*app.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := min(f.calls, len(f.results)-1)
	f.calls++
	if len(f.results) == 0 {
		return &app.Report{Outcome: app.NothingToDo}, nil
	}
	return f.results[i]()
}

func (f *fakeRun) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func ok() (*app.Report, error) { return &app.Report{Outcome: app.NothingToDo}, nil }

func start(t *testing.T, ad syncadapter.Adapter, f *fakeRun, mod func(*Config)) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{
		Run: f.run, Adapter: ad, Dir: t.TempDir(),
		Debounce: 50 * time.Millisecond, Poll: time.Hour, FallbackPoll: time.Hour, PendingRetry: 50 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mod != nil {
		mod(&cfg)
	}
	done := make(chan struct{})
	go func() {
		Loop(ctx, cfg)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("loop did not stop after cancel")
		}
	})
	return cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBurstGivesOneRun(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){ok}}
	ad := watchingAdapter{notify: make(chan func(string), 1)}
	start(t, ad, f, nil)
	notify := <-ad.notify
	waitFor(t, "initial run", func() bool { return f.count() == 1 })
	for i := 0; i < 5; i++ {
		notify("create")
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, "debounced run", func() bool { return f.count() == 2 })
	time.Sleep(200 * time.Millisecond)
	if f.count() != 2 {
		t.Errorf("runs = %d, want 2", f.count())
	}
}

func TestPollingWithoutWatcher(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){ok}}
	start(t, pollingAdapter{}, f, func(c *Config) { c.FallbackPoll = 40 * time.Millisecond })
	waitFor(t, "several polls", func() bool { return f.count() >= 3 })
}

func TestFallbackWhenNoEvents(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){ok}}
	ad := watchingAdapter{err: syncadapter.ErrNoEvents}
	start(t, ad, f, func(c *Config) { c.FallbackPoll = 40 * time.Millisecond })
	waitFor(t, "polling after watcher failure", func() bool { return f.count() >= 3 })
}

func TestPendingIsRetried(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){
		func() (*app.Report, error) { return &app.Report{Outcome: app.NothingToDo, Pending: 1}, nil },
		ok,
	}}
	start(t, pollingAdapter{}, f, nil)
	waitFor(t, "retry", func() bool { return f.count() == 2 })
	time.Sleep(1300 * time.Millisecond)
	if f.count() != 2 {
		t.Errorf("runs = %d, want 2", f.count())
	}
}

func TestGuardDoesNotLoop(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){
		func() (*app.Report, error) { return nil, app.ErrGuard },
	}}
	ad := watchingAdapter{notify: make(chan func(string), 1)}
	start(t, ad, f, nil)
	notify := <-ad.notify
	waitFor(t, "initial run", func() bool { return f.count() == 1 })
	time.Sleep(300 * time.Millisecond)
	if f.count() != 1 {
		t.Fatalf("guard error retried on its own: %d runs", f.count())
	}
	notify("create")
	waitFor(t, "run after new change", func() bool { return f.count() == 2 })
}

func TestFailingWatcherDoesNotTriggerRuns(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){ok}}
	ad := watchingAdapter{err: errors.New("boom")}
	start(t, ad, f, func(c *Config) { c.Restart = 10 * time.Millisecond })
	waitFor(t, "initial run", func() bool { return f.count() == 1 })
	time.Sleep(300 * time.Millisecond)
	if f.count() != 1 {
		t.Errorf("watcher restarts triggered %d extra runs", f.count()-1)
	}
}

func TestCancelStops(t *testing.T) {
	f := &fakeRun{results: []func() (*app.Report, error){ok}}
	cancel := start(t, pollingAdapter{}, f, nil)
	waitFor(t, "initial run", func() bool { return f.count() == 1 })
	cancel() // cleanup verifies that the loop returns
}
