package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ErrNotReady is returned by Ready while the service cannot serve data yet.
var ErrNotReady = errors.New("not ready")

type Store struct {
	db       *sql.DB
	migrated atomic.Bool
}

func New(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) DB() *sql.DB { return s.db }

// Migrate marks the store ready for queries after applying migrations.
func (s *Store) Migrate(ctx context.Context) ([]int64, error) {
	applied, err := Migrate(ctx, s.db)
	if err != nil {
		return nil, err
	}
	s.migrated.Store(true)
	return applied, nil
}

// Ready reports nil once migrations have run, the database answers, and at
// least one collector run has finished successfully at any time. A restart
// with existing data is therefore ready immediately.
func (s *Store) Ready(ctx context.Context) error {
	if !s.migrated.Load() {
		return fmt.Errorf("%w: migrations pending", ErrNotReady)
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("%w: database unreachable: %v", ErrNotReady, err)
	}
	ok, err := s.HasSuccessfulRun(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	if !ok {
		return fmt.Errorf("%w: no successful collector run yet", ErrNotReady)
	}
	return nil
}

func (s *Store) HasSuccessfulRun(ctx context.Context) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM collector_runs WHERE status IN ('ok', 'partial'))`).Scan(&ok)
	return ok, err
}

// RunStatus is the terminal state of a collector run. "partial" means at least
// one scope failed while others succeeded.
type RunStatus string

const (
	RunOK      RunStatus = "ok"
	RunPartial RunStatus = "partial"
	RunError   RunStatus = "error"
)

func (s *Store) StartRun(ctx context.Context, collector string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO collector_runs (collector, status) VALUES ($1, 'running') RETURNING id`,
		collector).Scan(&id)
	return id, err
}

func (s *Store) FinishRun(ctx context.Context, id int64, status RunStatus, items int, errText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE collector_runs SET finished_at = now(), status = $2, items = $3, error = NULLIF($4, '')
		 WHERE id = $1`,
		id, string(status), items, errText)
	return err
}

// FailStaleRuns marks runs left in 'running' by a killed process as errors.
// Called once on boot; a single replica means nothing else can be running.
func (s *Store) FailStaleRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE collector_runs SET finished_at = now(), status = 'error', error = 'interrupted by restart'
		 WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LastSuccess returns the finish time of each collector's latest ok or
// partial run.
func (s *Store) LastSuccess(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT collector, max(finished_at) FROM collector_runs
		 WHERE status IN ('ok', 'partial') GROUP BY collector`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var c string
		var t time.Time
		if err := rows.Scan(&c, &t); err != nil {
			return nil, err
		}
		out[c] = t
	}
	return out, rows.Err()
}
