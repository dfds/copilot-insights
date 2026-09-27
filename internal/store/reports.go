package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/report"
)

const (
	ReportComplete = "complete"
	ReportNoData   = "no_data"
)

// maxParams keeps a multi-row INSERT under Postgres's 65535 bind parameters.
const maxParams = 60000

// ReportStatuses returns the stored status of one report per day in from..to,
// keyed by YYYY-MM-DD. reportType is the API name, e.g. "users-1-day".
func (s *Store) ReportStatuses(ctx context.Context, scope github.Scope, reportType string, from, to time.Time) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day::text, status FROM copilot_report_raw
		 WHERE scope_kind = $1 AND scope_name = $2 AND report_type = $3 AND day BETWEEN $4::date AND $5::date`,
		string(scope.Kind), scope.Name, reportType, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var day, status string
		if err := rows.Scan(&day, &status); err != nil {
			return nil, err
		}
		out[day] = status
	}
	return out, rows.Err()
}

// SaveReportNoData records that GitHub had no data for the report and day.
func (s *Store) SaveReportNoData(ctx context.Context, scope github.Scope, reportType string, day time.Time) error {
	_, err := s.db.ExecContext(ctx, upsertRaw, string(scope.Kind), scope.Name, reportType,
		day.Format(time.DateOnly), ReportNoData, 0, nil)
	return err
}

const upsertRaw = `INSERT INTO copilot_report_raw (scope_kind, scope_name, report_type, day, status, row_count, payload)
	VALUES ($1, $2, $3, $4::date, $5, $6, $7)
	ON CONFLICT (scope_kind, scope_name, report_type, day) DO UPDATE SET
	  status = EXCLUDED.status, row_count = EXCLUDED.row_count, payload = EXCLUDED.payload, fetched_at = now()`

// SaveReport stores a downloaded report and its mapped rows in one
// transaction. The mapped rows replace what the scope holds for that report
// and day, so re-fetching a day is idempotent. day is the report day, or the
// end day for a 28-day report.
func (s *Store) SaveReport(ctx context.Context, scope github.Scope, reportType string, day time.Time,
	rows []json.RawMessage, p *report.Parsed) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	kind, name, date := string(scope.Kind), scope.Name, day.Format(time.DateOnly)
	var payload bytes.Buffer
	payload.WriteByte('[')
	for i, r := range rows {
		if i > 0 {
			payload.WriteByte(',')
		}
		payload.Write(r)
	}
	payload.WriteByte(']')
	if _, err := tx.ExecContext(ctx, upsertRaw, kind, name, reportType, date, ReportComplete, len(rows),
		payload.String()); err != nil {
		return fmt.Errorf("save raw report: %w", err)
	}

	del := func(table, extra string) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM `+table+
			` WHERE scope_kind = $1 AND scope_name = $2 AND day = $3::date`+extra, kind, name, date)
		if err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
		return nil
	}
	base := []any{kind, name, date}
	factCols := []string{"breakdown", "key1", "key2", "metric", "value"}

	switch {
	case p.Window == github.Window28Day:
		if err := del("copilot_daily_facts", ` AND breakdown = '`+report.BreakdownFeatureEngagement+`'`); err != nil {
			return err
		}
		if err := insertFacts(ctx, tx, "copilot_daily_facts", base, p.Engagement); err != nil {
			return err
		}

	case p.Type == github.ReportSummary:
		if err := del("copilot_daily", ""); err != nil {
			return err
		}
		if err := del("copilot_daily_facts", ` AND breakdown <> '`+report.BreakdownFeatureEngagement+`'`); err != nil {
			return err
		}
		if p.Daily != nil {
			cols := append([]string{"scope_kind", "scope_name", "day"}, report.DailyColumns...)
			vals := append(append(base, p.Daily.Values...), string(p.Daily.Raw))
			if err := insertRows(ctx, tx, "copilot_daily", append(cols, "raw"), [][]any{vals}); err != nil {
				return err
			}
			if err := insertFacts(ctx, tx, "copilot_daily_facts", base, p.Daily.Facts); err != nil {
				return err
			}
		}

	case p.Type == github.ReportUsers:
		if err := del("copilot_user_daily", ""); err != nil {
			return err
		}
		if err := del("copilot_user_daily_facts", ""); err != nil {
			return err
		}
		cols := append([]string{"scope_kind", "scope_name", "day", "user_id", "login"}, report.UserColumns...)
		cols = append(cols, "ides", "languages", "models", "raw")
		users := make([][]any, 0, len(p.Users))
		var facts [][]any
		for _, u := range p.Users {
			row := append(append(base, u.UserID, u.Login), u.Values...)
			users = append(users, append(row, u.IDEs, u.Languages, u.Models, string(u.Raw)))
			for _, f := range u.Facts {
				facts = append(facts, append(base, u.UserID, f.Breakdown, f.Key1, f.Key2, f.Metric, f.Value))
			}
		}
		if err := insertRows(ctx, tx, "copilot_user_daily", cols, users); err != nil {
			return err
		}
		fcols := append([]string{"scope_kind", "scope_name", "day", "user_id"}, factCols...)
		if err := insertRows(ctx, tx, "copilot_user_daily_facts", fcols, facts); err != nil {
			return err
		}

	case p.Type == github.ReportRepos:
		if err := del("copilot_repo_daily", ""); err != nil {
			return err
		}
		cols := append([]string{"scope_kind", "scope_name", "day", "repo_id", "organization_id",
			"repo_owner_name", "repo_name", "repo_visibility"}, report.RepoColumns...)
		repos := make([][]any, 0, len(p.Repos))
		for _, r := range p.Repos {
			row := append(append(base, r.RepoID, nullIfEmpty(r.OrganizationID), r.Owner, r.Name,
				nullIfEmpty(r.Visibility)), r.Values...)
			repos = append(repos, append(row, string(r.Raw)))
		}
		if err := insertRows(ctx, tx, "copilot_repo_daily", append(cols, "raw"), repos); err != nil {
			return err
		}

	case p.Type == github.ReportUserTeams:
		if err := del("copilot_user_teams", ""); err != nil {
			return err
		}
		teams := make([][]any, 0, len(p.Teams))
		for _, t := range p.Teams {
			teams = append(teams, append(base, t.TeamID, t.UserID, t.Slug, t.Login))
		}
		if err := insertRows(ctx, tx, "copilot_user_teams",
			[]string{"scope_kind", "scope_name", "day", "team_id", "user_id", "team_slug", "login"}, teams); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unsupported report %s %s", p.Type, p.Window)
	}
	return tx.Commit()
}

func insertFacts(ctx context.Context, tx *sql.Tx, table string, base []any, facts []report.Fact) error {
	rows := make([][]any, 0, len(facts))
	for _, f := range facts {
		rows = append(rows, append(base, f.Breakdown, f.Key1, f.Key2, f.Metric, f.Value))
	}
	return insertRows(ctx, tx, table,
		[]string{"scope_kind", "scope_name", "day", "breakdown", "key1", "key2", "metric", "value"}, rows)
}

// Callers delete the day's rows first, so a conflict can only be a duplicate
// within one report.
func insertRows(ctx context.Context, tx *sql.Tx, table string, cols []string, rows [][]any) error {
	return writeRows(ctx, tx, table, cols, rows, "ON CONFLICT DO NOTHING")
}

// Callers merge duplicate keys before DO UPDATE, which rejects two matching
// rows in one statement.
func writeRows(ctx context.Context, tx *sql.Tx, table string, cols []string, rows [][]any, onConflict string) error {
	per := max(1, maxParams/len(cols))
	for start := 0; start < len(rows); start += per {
		chunk := rows[start:min(start+per, len(rows))]
		var q strings.Builder
		q.WriteString("INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES ")
		args := make([]any, 0, len(chunk)*len(cols))
		for i, row := range chunk {
			if len(row) != len(cols) {
				return fmt.Errorf("insert %s: row has %d values for %d columns", table, len(row), len(cols))
			}
			if i > 0 {
				q.WriteString(", ")
			}
			q.WriteByte('(')
			for j := range row {
				if j > 0 {
					q.WriteString(", ")
				}
				q.WriteString("$" + strconv.Itoa(len(args)+j+1))
			}
			q.WriteByte(')')
			args = append(args, row...)
		}
		q.WriteString(" " + onConflict)
		if _, err := tx.ExecContext(ctx, q.String(), args...); err != nil {
			return fmt.Errorf("insert %s: %w", table, err)
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
