package store

import (
	"context"
	"time"
)

// PruneRawReports deletes copilot_report_raw rows whose report day is before
// cutoff. It is the only retention rule; normalized tables are kept.
func (s *Store) PruneRawReports(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM copilot_report_raw WHERE day < $1`,
		cutoff.UTC().Format(time.DateOnly))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
