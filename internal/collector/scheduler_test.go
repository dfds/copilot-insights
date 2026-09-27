package collector

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

func TestClassify(t *testing.T) {
	scopeErr := []error{errors.New("org:a: 500")}
	for _, tc := range []struct {
		name string
		out  Outcome
		err  error
		want store.RunStatus
	}{
		{"clean", Outcome{Scopes: 2}, nil, store.RunOK},
		{"no scopes", Outcome{}, nil, store.RunOK},
		{"one of two scopes failed", Outcome{Scopes: 2, Errors: scopeErr}, nil, store.RunPartial},
		{"every scope failed", Outcome{Scopes: 1, Errors: scopeErr}, nil, store.RunError},
		{"run error", Outcome{Scopes: 2}, errors.New("db down"), store.RunError},
	} {
		if got, _ := classify(tc.out, tc.err); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}

	_, text := classify(Outcome{Scopes: 3, Errors: []error{errors.New(strings.Repeat("é", maxErrorText))}}, nil)
	if !utf8.ValidString(text) || len(text) > maxErrorText+len("…") {
		t.Errorf("long error text not cut to valid UTF-8 (len %d)", len(text))
	}
}

func newTestScheduler(runs *fakeRuns) *Scheduler {
	s := NewScheduler(runs, zap.NewNop(), newTestMetrics())
	s.rand = func() float64 { return 0 }
	return s
}

func TestRunNowRecordsRun(t *testing.T) {
	runs := &fakeRuns{}
	s := newTestScheduler(runs)
	s.Add(Job{Name: "seats", Interval: time.Hour, Run: func(context.Context) (Outcome, error) {
		return Outcome{Items: 5, Scopes: 2, Errors: []error{errors.New("org:x: 502")}}, nil
	}})
	s.Add(Job{Name: "boom", Interval: time.Hour, Run: func(context.Context) (Outcome, error) {
		panic("nil map")
	}})

	if err := s.RunNow(context.Background(), "seats"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(context.Background(), "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(context.Background(), "nope"); !errors.Is(err, ErrUnknownCollector) {
		t.Errorf("unknown: %v", err)
	}

	got := runs.all()
	if len(got) != 2 {
		t.Fatalf("runs = %+v", got)
	}
	if r := got[0]; r.status != store.RunPartial || r.items != 5 || r.errText != "org:x: 502" || !r.finished {
		t.Errorf("seats run = %+v", r)
	}
	if r := got[1]; r.status != store.RunError || r.errText != "panic: nil map" {
		t.Errorf("panicking run = %+v", r)
	}
	if n := testutil.ToFloat64(s.metrics.CollectorRuns.WithLabelValues("seats", "partial")); n != 1 {
		t.Errorf("runs metric = %v", n)
	}
	if n := testutil.ToFloat64(s.metrics.CollectorItems.WithLabelValues("seats")); n != 5 {
		t.Errorf("items metric = %v", n)
	}
}

func TestRunClosedAfterShutdown(t *testing.T) {
	runs := &fakeRuns{}
	s := newTestScheduler(runs)
	ctx, cancel := context.WithCancel(context.Background())
	s.Add(Job{Name: "members", Interval: time.Hour, Run: func(ctx context.Context) (Outcome, error) {
		cancel()
		return Outcome{}, ctx.Err()
	}})
	_ = s.RunNow(ctx, "members")
	if r := runs.all()[0]; !r.finished || r.status != store.RunError || r.errText != "context canceled" {
		t.Errorf("run = %+v", r)
	}
}

func TestTriggerRejectsOverlap(t *testing.T) {
	runs := &fakeRuns{}
	s := newTestScheduler(runs)
	release := make(chan struct{})
	started := make(chan struct{})
	s.Add(Job{Name: "seats", Interval: time.Hour, Run: func(context.Context) (Outcome, error) {
		close(started)
		<-release
		return Outcome{}, nil
	}})

	if err := s.Trigger("seats"); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := s.Trigger("seats"); !errors.Is(err, ErrBusy) {
		t.Errorf("second trigger: %v, want ErrBusy", err)
	}
	if err := s.RunNow(context.Background(), "seats"); !errors.Is(err, ErrBusy) {
		t.Errorf("RunNow while running: %v, want ErrBusy", err)
	}
	if err := s.Trigger("nope"); !errors.Is(err, ErrUnknownCollector) {
		t.Errorf("unknown: %v", err)
	}
	close(release)
	s.Wait()
	if got := runs.all(); len(got) != 1 || got[0].status != store.RunOK {
		t.Errorf("runs = %+v", got)
	}
}

func TestStartRunsPrimeJobFirst(t *testing.T) {
	runs := &fakeRuns{}
	s := newTestScheduler(runs)
	var mu sync.Mutex
	var order []string
	done := make(chan struct{}, 3)
	job := func(name string) RunFunc {
		return func(context.Context) (Outcome, error) {
			if name == "members" {
				time.Sleep(20 * time.Millisecond) // the others must wait for it
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			done <- struct{}{}
			return Outcome{}, nil
		}
	}
	s.Add(Job{Name: "seats", Interval: time.Hour, Run: job("seats")})
	s.Add(Job{Name: "members", Interval: time.Hour, Run: job("members"), Prime: true})
	s.Add(Job{Name: "retention", Interval: time.Hour, Run: job("retention")})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx, true)
	for range 3 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("jobs did not run")
		}
	}
	cancel()
	s.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "members" {
		t.Errorf("order = %v, want members first and each job once", order)
	}
}

func TestStartWithoutRunOnStartWaitsAnInterval(t *testing.T) {
	runs := &fakeRuns{}
	s := newTestScheduler(runs)
	s.Add(Job{Name: "seats", Interval: time.Hour, Run: func(context.Context) (Outcome, error) {
		return Outcome{}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx, false)
	time.Sleep(20 * time.Millisecond)
	cancel()
	s.Wait()
	if n := len(runs.all()); n != 0 {
		t.Errorf("%d runs before the first interval elapsed", n)
	}
}

func TestJitter(t *testing.T) {
	s := newTestScheduler(&fakeRuns{})
	for _, r := range []float64{0, 0.5, 0.999} {
		s.rand = func() float64 { return r }
		got := s.jittered(time.Hour)
		if got < time.Hour || got >= time.Hour+6*time.Minute {
			t.Errorf("rand=%v: %s outside [1h, 1h6m)", r, got)
		}
	}
}
