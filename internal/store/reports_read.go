package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/report"
)

// Report read models. Metric names inside values, totals and metrics objects
// are GitHub's report field names (snake_case), so they match the upstream
// documentation; everything around them is camelCase like the other models.

// FactRow is one breakdown key's metrics. Day is empty for range totals.
type FactRow struct {
	Day       string             `json:"day,omitempty"`
	Breakdown string             `json:"breakdown"`
	Key1      string             `json:"key1,omitempty"`
	Key2      string             `json:"key2,omitempty"`
	Metrics   map[string]float64 `json:"metrics"`
}

// Engagement is the 28-day copilot_feature_engagement block ending PeriodEnd.
type Engagement struct {
	PeriodEnd string             `json:"periodEnd"`
	Metrics   map[string]float64 `json:"metrics"`
	Features  []FactRow          `json:"features"`
}

// UserTotals aggregates a user's users-1-day rows over a range. Totals sums
// the additive counters and ORs the used_* flags.
type UserTotals struct {
	UserID        int64           `json:"userId"`
	Login         string          `json:"login"`
	Name          *string         `json:"name"`
	Email         *string         `json:"email"`
	DaysActive    int             `json:"daysActive"`
	FirstDay      string          `json:"firstDay"`
	LastDay       string          `json:"lastDay"`
	AdoptionPhase *string         `json:"adoptionPhase"`
	Totals        json.RawMessage `json:"totals"`
}

type RepoTotals struct {
	RepoID       int64           `json:"repoId"`
	Owner        string          `json:"owner"`
	Name         string          `json:"name"`
	Visibility   *string         `json:"visibility"`
	DaysWithData int             `json:"daysWithData"`
	Totals       json.RawMessage `json:"totals"`
}

// TeamDay is one team on one day. Members comes from the user-teams report;
// ActiveUsers counts the members present in that day's users report, and
// Totals sums their counters and counts their used_* flags.
type TeamDay struct {
	Day         string          `json:"day"`
	TeamID      int64           `json:"teamId"`
	Slug        string          `json:"slug"`
	Members     int             `json:"members"`
	ActiveUsers int             `json:"activeUsers"`
	Totals      json.RawMessage `json:"totals"`
}

type SeatCounts struct {
	Total               int `json:"total"`
	Active28d           int `json:"active28d"`
	NeverActive         int `json:"neverActive"`
	PendingCancellation int `json:"pendingCancellation"`
}

// Summary is the headline view of one scope over a range.
type Summary struct {
	// Seats is the current seat state; nil when no seats are collected for
	// the scope (enterprise without a PAT).
	Seats *SeatCounts `json:"seats"`
	// LatestDay is the most recent summary report day in the range.
	LatestDay    json.RawMessage `json:"latestDay"`
	DaysWithData int             `json:"daysWithData"`
	// AvgDailyActiveUsers averages daily_active_users over DaysWithData.
	AvgDailyActiveUsers *float64 `json:"avgDailyActiveUsers"`
	// ActiveUsers counts distinct users in the users reports of the range.
	ActiveUsers int `json:"activeUsers"`
	// Totals sums the additive summary counters over the range, plus
	// ai_credits_used from the users reports.
	Totals            json.RawMessage `json:"totals"`
	FeatureEngagement *Engagement     `json:"featureEngagement"`
}

type UserDailyFilter struct {
	Scope  github.Scope
	From   time.Time
	To     time.Time
	UserID int64
	Login  string
	Page   Page
}

func dates(from, to time.Time) (string, string) {
	return from.Format(time.DateOnly), to.Format(time.DateOnly)
}

func sumObject(prefix string, cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, "'"+c+"', sum("+prefix+c+")")
	}
	return "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
}

func flagObject(prefix string, count bool) string {
	parts := make([]string, 0, len(report.UserFlagColumns))
	for _, c := range report.UserFlagColumns {
		expr := "coalesce(bool_or(" + prefix + c + "), false)"
		if count {
			expr = "count(*) FILTER (WHERE " + prefix + c + ")"
		}
		parts = append(parts, "'"+c+"', "+expr)
	}
	return "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
}

const rowColumnsDropped = `ARRAY['scope_kind', 'scope_name', 'raw', 'fetched_at']`

func scanObjects(rows *sql.Rows, withTotal bool) ([]json.RawMessage, int, error) {
	defer rows.Close()
	out := []json.RawMessage{}
	total := 0
	for rows.Next() {
		var v []byte
		dst := []any{&v}
		if withTotal {
			dst = append(dst, &total)
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// rollingActive counts distinct users in the scope's users reports over the n
// days ending d.day. It is null when the window starts before the first stored
// users day, where the count would be partial.
func rollingActive(n int) string {
	back := strconv.Itoa(n - 1)
	return `CASE WHEN d.day - ` + back + ` >= f.first_day THEN (
		SELECT count(DISTINCT u.user_id) FROM copilot_user_daily u
		WHERE u.scope_kind = d.scope_kind AND u.scope_name = d.scope_name
		  AND u.day BETWEEN d.day - ` + back + ` AND d.day) END`
}

// DailyReports returns the scope's summary report rows in from..to as JSON
// objects with day and every column the report carried. GitHub's weekly and
// monthly active user counts restart each calendar week (Sunday) and month, so
// rolling_7d_active_users and rolling_28d_active_users are added from the
// users reports.
func (s *Store) DailyReports(ctx context.Context, scope github.Scope, from, to time.Time) ([]json.RawMessage, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT jsonb_strip_nulls((to_jsonb(d) - `+rowColumnsDropped+`) || jsonb_build_object(
		   'rolling_7d_active_users', `+rollingActive(7)+`,
		   'rolling_28d_active_users', `+rollingActive(28)+`))
		 FROM copilot_daily d
		 CROSS JOIN (SELECT min(day) AS first_day FROM copilot_user_daily
		             WHERE scope_kind = $1 AND scope_name = $2) f
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date ORDER BY day`,
		string(scope.Kind), scope.Name, f, t)
	if err != nil {
		return nil, err
	}
	out, _, err := scanObjects(rows, false)
	return out, err
}

func scanFacts(rows *sql.Rows) ([]FactRow, error) {
	defer rows.Close()
	out := []FactRow{}
	for rows.Next() {
		var f FactRow
		if err := rows.Scan(&f.Day, &f.Breakdown, &f.Key1, &f.Key2, jsonCol{&f.Metrics}); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DailyFacts returns the scope's summary breakdowns in from..to, one row per
// day and key. The 28-day feature engagement is excluded; see Engagement.
func (s *Store) DailyFacts(ctx context.Context, scope github.Scope, from, to time.Time, breakdowns []string) ([]FactRow, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT day::text, breakdown, key1, key2, jsonb_object_agg(metric, value)
		 FROM copilot_daily_facts
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date
		   AND breakdown = ANY($5) AND breakdown <> $6
		 GROUP BY day, breakdown, key1, key2 ORDER BY day, breakdown, key1, key2`,
		string(scope.Kind), scope.Name, f, t, breakdowns, report.BreakdownFeatureEngagement)
	if err != nil {
		return nil, err
	}
	return scanFacts(rows)
}

// Engagement returns the latest feature engagement block ending on or before
// to, or nil when none is stored.
func (s *Store) Engagement(ctx context.Context, scope github.Scope, to time.Time) (*Engagement, error) {
	rows, err := s.db.QueryContext(ctx,
		`WITH latest AS (
		   SELECT max(day) AS day FROM copilot_daily_facts
		   WHERE scope_kind = $1 AND scope_name = $2 AND breakdown = $3 AND day <= $4::date)
		 SELECT f.day::text, f.breakdown, f.key1, f.key2, jsonb_object_agg(f.metric, f.value)
		 FROM copilot_daily_facts f JOIN latest l ON f.day = l.day
		 WHERE f.scope_kind = $1 AND f.scope_name = $2 AND f.breakdown = $3
		 GROUP BY f.day, f.breakdown, f.key1, f.key2 ORDER BY f.key1`,
		string(scope.Kind), scope.Name, report.BreakdownFeatureEngagement, to.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	facts, err := scanFacts(rows)
	if err != nil || len(facts) == 0 {
		return nil, err
	}
	e := &Engagement{PeriodEnd: facts[0].Day, Metrics: map[string]float64{}, Features: []FactRow{}}
	for _, f := range facts {
		f.Day, f.Breakdown = "", ""
		if f.Key1 == "" {
			e.Metrics = f.Metrics
			continue
		}
		e.Features = append(e.Features, f)
	}
	return e, nil
}

// UserDaily lists users-1-day rows ordered by day and login, with the total
// before paging.
func (s *Store) UserDaily(ctx context.Context, f UserDailyFilter) ([]json.RawMessage, int, error) {
	from, to := dates(f.From, f.To)
	rows, err := s.db.QueryContext(ctx,
		`SELECT jsonb_strip_nulls(to_jsonb(u) - `+rowColumnsDropped+`), count(*) OVER ()
		 FROM copilot_user_daily u
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date
		   AND ($5::bigint = 0 OR user_id = $5) AND ($6 = '' OR lower(login) = lower($6))
		 ORDER BY day, lower(login), user_id LIMIT $7 OFFSET $8`,
		string(f.Scope.Kind), f.Scope.Name, from, to, f.UserID, f.Login, f.Page.Limit, f.Page.Offset)
	if err != nil {
		return nil, 0, err
	}
	return scanObjects(rows, true)
}

// UserFacts returns one user's breakdowns per day in from..to.
func (s *Store) UserFacts(ctx context.Context, scope github.Scope, userID int64, from, to time.Time, breakdowns []string) ([]FactRow, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT day::text, breakdown, key1, key2, jsonb_object_agg(metric, value)
		 FROM copilot_user_daily_facts
		 WHERE scope_kind = $1 AND scope_name = $2 AND user_id = $3 AND day BETWEEN $4::date AND $5::date
		   AND breakdown = ANY($6)
		 GROUP BY day, breakdown, key1, key2 ORDER BY day, breakdown, key1, key2`,
		string(scope.Kind), scope.Name, userID, f, t, breakdowns)
	if err != nil {
		return nil, err
	}
	return scanFacts(rows)
}

// UserFactTotals sums one user's breakdowns over from..to, one row per key.
func (s *Store) UserFactTotals(ctx context.Context, scope github.Scope, userID int64, from, to time.Time, breakdowns []string) ([]FactRow, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT '', breakdown, key1, key2, jsonb_object_agg(metric, total) FROM (
		   SELECT breakdown, key1, key2, metric, sum(value) AS total
		   FROM copilot_user_daily_facts
		   WHERE scope_kind = $1 AND scope_name = $2 AND user_id = $3 AND day BETWEEN $4::date AND $5::date
		     AND breakdown = ANY($6)
		   GROUP BY breakdown, key1, key2, metric) m
		 GROUP BY breakdown, key1, key2 ORDER BY breakdown, key1, key2`,
		string(scope.Kind), scope.Name, userID, f, t, breakdowns)
	if err != nil {
		return nil, err
	}
	return scanFacts(rows)
}

// UserTotals aggregates users-1-day rows per user over from..to, ordered by
// login. userID 0 and login "" match every user.
func (s *Store) UserTotals(ctx context.Context, scope github.Scope, from, to time.Time, userID int64, login string, p Page) ([]UserTotals, int, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.user_id, a.login, g.name, g.primary_email, a.days, a.first_day, a.last_day, a.phase,
		   a.sums || a.flags, count(*) OVER ()
		 FROM (
		   SELECT user_id, (array_agg(login ORDER BY day DESC))[1] AS login, count(*) AS days,
		     min(day)::text AS first_day, max(day)::text AS last_day,
		     (array_agg(ai_adoption_phase ORDER BY day DESC) FILTER (WHERE ai_adoption_phase IS NOT NULL))[1] AS phase,
		     jsonb_strip_nulls(`+sumObject("", report.UserSumColumns)+`) AS sums,
		     `+flagObject("", false)+` AS flags
		   FROM copilot_user_daily
		   WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date
		     AND ($5::bigint = 0 OR user_id = $5) AND ($6 = '' OR lower(login) = lower($6))
		   GROUP BY user_id) a
		 LEFT JOIN github_users g ON g.user_id = a.user_id
		 ORDER BY lower(a.login), a.user_id LIMIT $7 OFFSET $8`,
		string(scope.Kind), scope.Name, f, t, userID, login, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UserTotals{}
	total := 0
	for rows.Next() {
		var u UserTotals
		var v []byte
		if err := rows.Scan(&u.UserID, &u.Login, &u.Name, &u.Email, &u.DaysActive, &u.FirstDay, &u.LastDay,
			&u.AdoptionPhase, &v, &total); err != nil {
			return nil, 0, err
		}
		u.Totals = v
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// RepoDaily lists repos-1-day rows ordered by day and repository. repo
// matches "owner/name" or the name alone, case-insensitively.
func (s *Store) RepoDaily(ctx context.Context, scope github.Scope, from, to time.Time, repo string, p Page) ([]json.RawMessage, int, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT jsonb_strip_nulls(to_jsonb(r) - `+rowColumnsDropped+`), count(*) OVER ()
		 FROM copilot_repo_daily r
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date
		   AND ($5 = '' OR lower(repo_name) = lower($5) OR lower(repo_owner_name || '/' || repo_name) = lower($5))
		 ORDER BY day, lower(repo_owner_name), lower(repo_name), repo_id LIMIT $6 OFFSET $7`,
		string(scope.Kind), scope.Name, f, t, repo, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, err
	}
	return scanObjects(rows, true)
}

// RepoTotals sums the PR counters per repository over from..to, busiest
// (most PRs created) first.
func (s *Store) RepoTotals(ctx context.Context, scope github.Scope, from, to time.Time, p Page) ([]RepoTotals, int, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo_id, (array_agg(repo_owner_name ORDER BY day DESC))[1], (array_agg(repo_name ORDER BY day DESC))[1],
		   (array_agg(repo_visibility ORDER BY day DESC))[1], count(*),
		   jsonb_strip_nulls(`+sumObject("", report.RepoSumColumns)+`), count(*) OVER ()
		 FROM copilot_repo_daily
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date
		 GROUP BY repo_id
		 ORDER BY sum(pr_total_created) DESC NULLS LAST, repo_id LIMIT $5 OFFSET $6`,
		string(scope.Kind), scope.Name, f, t, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []RepoTotals{}
	total := 0
	for rows.Next() {
		var r RepoTotals
		var v []byte
		if err := rows.Scan(&r.RepoID, &r.Owner, &r.Name, &r.Visibility, &r.DaysWithData, &v, &total); err != nil {
			return nil, 0, err
		}
		r.Totals = v
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// TeamDaily joins an org's user-teams rows to its users rows per day and
// team. team matches the slug; "" matches every team.
func (s *Store) TeamDaily(ctx context.Context, org string, from, to time.Time, team string) ([]TeamDay, error) {
	f, t := dates(from, to)
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.day::text, t.team_id, (array_agg(t.team_slug))[1], count(*), count(u.user_id),
		   jsonb_strip_nulls(`+sumObject("u.", report.UserSumColumns)+`) || `+flagObject("u.", true)+`
		 FROM copilot_user_teams t
		 LEFT JOIN copilot_user_daily u ON u.scope_kind = t.scope_kind AND u.scope_name = t.scope_name
		   AND u.day = t.day AND u.user_id = t.user_id
		 WHERE t.scope_kind = 'org' AND t.scope_name = $1 AND t.day BETWEEN $2::date AND $3::date
		   AND ($4 = '' OR lower(t.team_slug) = lower($4))
		 GROUP BY t.day, t.team_id ORDER BY t.day, 3`,
		org, f, t, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamDay{}
	for rows.Next() {
		var d TeamDay
		var v []byte
		if err := rows.Scan(&d.Day, &d.TeamID, &d.Slug, &d.Members, &d.ActiveUsers, &v); err != nil {
			return nil, err
		}
		d.Totals = v
		out = append(out, d)
	}
	return out, rows.Err()
}

// Summary computes the headline figures of a scope over from..to.
func (s *Store) Summary(ctx context.Context, scope github.Scope, from, to time.Time) (*Summary, error) {
	kind, name := string(scope.Kind), scope.Name
	f, t := dates(from, to)
	sum := &Summary{}

	var seats SeatCounts
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE last_activity_at >= now() - interval '28 days'),
		   count(*) FILTER (WHERE last_activity_at IS NULL), count(*) FILTER (WHERE pending_cancellation_date IS NOT NULL)
		 FROM copilot_seats_current WHERE scope_kind = $1 AND scope_name = $2`,
		kind, name).Scan(&seats.Total, &seats.Active28d, &seats.NeverActive, &seats.PendingCancellation); err != nil {
		return nil, err
	}
	if seats.Total > 0 {
		sum.Seats = &seats
	}

	var totals []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*), avg(daily_active_users)::float8, jsonb_strip_nulls(`+sumObject("", report.DailySumColumns)+`)
		 FROM copilot_daily WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date`,
		kind, name, f, t).Scan(&sum.DaysWithData, &sum.AvgDailyActiveUsers, &totals); err != nil {
		return nil, err
	}

	var credits sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(DISTINCT user_id), sum(ai_credits_used)::float8
		 FROM copilot_user_daily WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date`,
		kind, name, f, t).Scan(&sum.ActiveUsers, &credits); err != nil {
		return nil, err
	}
	merged := map[string]any{}
	if err := json.Unmarshal(totals, &merged); err != nil {
		return nil, err
	}
	if credits.Valid {
		merged["ai_credits_used"] = credits.Float64
	}
	var err error
	if sum.Totals, err = json.Marshal(merged); err != nil {
		return nil, err
	}

	var latest sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT max(day)::text FROM copilot_daily
		 WHERE scope_kind = $1 AND scope_name = $2 AND day BETWEEN $3::date AND $4::date`,
		kind, name, f, t).Scan(&latest); err != nil {
		return nil, err
	}
	if latest.Valid {
		d, err := time.Parse(time.DateOnly, latest.String)
		if err != nil {
			return nil, err
		}
		days, err := s.DailyReports(ctx, scope, d, d)
		if err != nil {
			return nil, err
		}
		if len(days) > 0 {
			sum.LatestDay = days[0]
		}
	}

	if sum.FeatureEngagement, err = s.Engagement(ctx, scope, to); err != nil {
		return nil, err
	}
	return sum, nil
}

// HasUserReports reports whether any users-1-day rows are stored for scope.
func (s *Store) HasUserReports(ctx context.Context, scope github.Scope) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM copilot_user_daily WHERE scope_kind = $1 AND scope_name = $2)`,
		string(scope.Kind), scope.Name).Scan(&ok)
	return ok, err
}

// UserReportOrg picks the org whose users reports hold the most rows for the
// user in from..to (ties: org name), or "" when none do.
func (s *Store) UserReportOrg(ctx context.Context, userID int64, from, to time.Time) (string, error) {
	f, t := dates(from, to)
	var org string
	err := s.db.QueryRowContext(ctx,
		`SELECT scope_name FROM copilot_user_daily
		 WHERE scope_kind = 'org' AND user_id = $1 AND day BETWEEN $2::date AND $3::date
		 GROUP BY scope_name ORDER BY count(*) DESC, scope_name LIMIT 1`,
		userID, f, t).Scan(&org)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return org, err
}
