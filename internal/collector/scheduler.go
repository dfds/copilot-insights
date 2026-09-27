// Package collector runs the scheduled GitHub-to-Postgres collectors.
package collector

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"go.dfds.cloud/copilot-insights/internal/metrics"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

var (
	ErrUnknownCollector = errors.New("unknown collector")
	ErrBusy             = errors.New("collector is already running")
)

const (
	// startSpread bounds the random delay before a non-prime job's first run
	// when RunOnStart is set, so a restart does not fire every collector at once.
	startSpread   = 30 * time.Second
	finishTimeout = 10 * time.Second
	maxErrorText  = 2000
)

// Outcome is a collector run's result. Errors holds per-scope failures; the
// run is partial when some scopes failed and an error when all did.
type Outcome struct {
	Items  int
	Scopes int
	Errors []error
}

func (o *Outcome) fail(scope string, err error) {
	o.Errors = append(o.Errors, fmt.Errorf("%s: %w", scope, err))
}

type RunFunc func(ctx context.Context) (Outcome, error)

type Job struct {
	Name     string
	Interval time.Duration
	Run      RunFunc
	// Prime jobs run to completion before the others start when RunOnStart is
	// set. Members is prime: it discovers the scopes the others iterate.
	Prime bool
}

// RunRecorder persists collector_runs rows.
type RunRecorder interface {
	StartRun(ctx context.Context, collector string) (int64, error)
	FinishRun(ctx context.Context, id int64, status store.RunStatus, items int, errText string) error
}

type job struct {
	Job
	mu sync.Mutex
}

// Scheduler runs each job in its own goroutine every Interval plus up to 10%
// jitter. A job never overlaps itself: a tick or trigger that finds it running
// is skipped.
type Scheduler struct {
	runs    RunRecorder
	logger  *zap.Logger
	metrics *metrics.Metrics
	rand    func() float64

	jobs   []*job
	byName map[string]*job

	ctxMu sync.Mutex
	ctx   context.Context // runs started by Trigger; set by Start
	wg    sync.WaitGroup
}

func NewScheduler(runs RunRecorder, logger *zap.Logger, m *metrics.Metrics) *Scheduler {
	return &Scheduler{
		runs:    runs,
		logger:  logger,
		metrics: m,
		rand:    rand.Float64,
		byName:  map[string]*job{},
		ctx:     context.Background(),
	}
}

// Add registers a job. Call before Start.
func (s *Scheduler) Add(j Job) {
	jb := &job{Job: j}
	s.jobs = append(s.jobs, jb)
	s.byName[j.Name] = jb
}

func (s *Scheduler) Names() []string {
	out := make([]string, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j.Name)
	}
	return out
}

// Start launches the job loops and returns immediately. With runOnStart, prime
// jobs run first and the rest start within startSpread; otherwise every job
// first runs after one jittered interval. Loops stop when ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context, runOnStart bool) {
	s.ctxMu.Lock()
	s.ctx = ctx
	s.ctxMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if runOnStart {
			for _, j := range s.jobs {
				if j.Prime {
					_ = s.RunNow(ctx, j.Name)
				}
			}
		}
		for _, j := range s.jobs {
			first := s.jittered(j.Interval)
			if runOnStart && !j.Prime {
				first = time.Duration(s.rand() * float64(startSpread))
			}
			s.wg.Add(1)
			go s.loop(ctx, j, first)
		}
	}()
}

// Wait blocks until every loop and triggered run has returned.
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) loop(ctx context.Context, j *job, delay time.Duration) {
	defer s.wg.Done()
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.RunNow(ctx, j.Name); errors.Is(err, ErrBusy) {
			s.logger.Info("collector still running, tick skipped", zap.String("collector", j.Name))
		}
		t.Reset(s.jittered(j.Interval))
	}
}

func (s *Scheduler) jittered(d time.Duration) time.Duration {
	return d + time.Duration(s.rand()*float64(d)/10)
}

// Trigger starts a run in the background on the scheduler's context and
// returns at once: ErrBusy if the job is running, ErrUnknownCollector if the
// name is not registered.
func (s *Scheduler) Trigger(name string) error {
	j, ok := s.byName[name]
	if !ok {
		return ErrUnknownCollector
	}
	if !j.mu.TryLock() {
		return ErrBusy
	}
	s.ctxMu.Lock()
	ctx := s.ctx
	s.ctxMu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer j.mu.Unlock()
		s.execute(ctx, j)
	}()
	return nil
}

// RunNow runs a job synchronously.
func (s *Scheduler) RunNow(ctx context.Context, name string) error {
	j, ok := s.byName[name]
	if !ok {
		return ErrUnknownCollector
	}
	if !j.mu.TryLock() {
		return ErrBusy
	}
	defer j.mu.Unlock()
	s.execute(ctx, j)
	return nil
}

func (s *Scheduler) execute(ctx context.Context, j *job) {
	log := s.logger.With(zap.String("collector", j.Name))
	id, err := s.runs.StartRun(ctx, j.Name)
	if err != nil {
		log.Error("could not record collector run start; run skipped", zap.Error(err))
		s.metrics.CollectorRuns.WithLabelValues(j.Name, string(store.RunError)).Inc()
		return
	}
	started := time.Now()
	out, err := s.safeRun(ctx, j)
	elapsed := time.Since(started)
	status, errText := classify(out, err)

	// The run row is closed even when ctx was cancelled by shutdown.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if ferr := s.runs.FinishRun(fctx, id, status, out.Items, errText); ferr != nil {
		log.Error("could not record collector run result", zap.Error(ferr))
	}

	s.metrics.CollectorRuns.WithLabelValues(j.Name, string(status)).Inc()
	s.metrics.CollectorDuration.WithLabelValues(j.Name).Observe(elapsed.Seconds())
	s.metrics.CollectorItems.WithLabelValues(j.Name).Set(float64(out.Items))
	fields := []zap.Field{zap.String("status", string(status)), zap.Int("items", out.Items), zap.Duration("took", elapsed)}
	switch status {
	case store.RunOK:
		s.metrics.CollectorLastSuccess.WithLabelValues(j.Name).SetToCurrentTime()
		log.Info("collector run finished", fields...)
	case store.RunPartial:
		s.metrics.CollectorLastSuccess.WithLabelValues(j.Name).SetToCurrentTime()
		log.Warn("collector run finished with scope errors", append(fields, zap.String("error", errText))...)
	default:
		log.Error("collector run failed", append(fields, zap.String("error", errText))...)
	}
}

func (s *Scheduler) safeRun(ctx context.Context, j *job) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return j.Run(ctx)
}

func classify(out Outcome, err error) (store.RunStatus, string) {
	msgs := make([]string, 0, len(out.Errors)+1)
	if err != nil {
		msgs = append(msgs, err.Error())
	}
	for _, e := range out.Errors {
		msgs = append(msgs, e.Error())
	}
	text := strings.Join(msgs, "; ")
	if len(text) > maxErrorText {
		// Postgres rejects invalid UTF-8, which a byte cut can produce.
		text = strings.ToValidUTF8(text[:maxErrorText], "") + "…"
	}
	switch {
	case err != nil:
		return store.RunError, text
	case len(out.Errors) == 0:
		return store.RunOK, ""
	case len(out.Errors) >= out.Scopes:
		return store.RunError, text
	default:
		return store.RunPartial, text
	}
}
