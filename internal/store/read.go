package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
)

// JSON tags define the API wire format.

type ScopeInfo struct {
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	Installed      bool      `json:"installed"`
	Active         bool      `json:"active"`
	CopilotEnabled bool      `json:"copilotEnabled"`
	FirstSeenAt    time.Time `json:"firstSeenAt"`
	LastSeenAt     time.Time `json:"lastSeenAt"`
}

func (s ScopeInfo) Scope() github.Scope {
	return github.Scope{Kind: github.ScopeKind(s.Kind), Name: s.Name}
}

type Run struct {
	ID         int64      `json:"id"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Items      int        `json:"items"`
	Error      *string    `json:"error"`
}

type CollectorStatus struct {
	Collector string     `json:"collector"`
	LastRun   *Run       `json:"lastRun"`
	LastOkAt  *time.Time `json:"lastOkAt"`
}

type Seat struct {
	Scope                   string     `json:"scope"`
	UserID                  int64      `json:"userId"`
	Login                   string     `json:"login"`
	Name                    *string    `json:"name"`
	Email                   *string    `json:"email"`
	OrgName                 *string    `json:"orgName"`
	AssigningTeamSlug       *string    `json:"assigningTeamSlug"`
	PlanType                *string    `json:"planType"`
	CreatedAt               *time.Time `json:"createdAt"`
	UpdatedAt               *time.Time `json:"updatedAt"`
	LastActivityAt          *time.Time `json:"lastActivityAt"`
	LastActivityEditor      *string    `json:"lastActivityEditor"`
	LastAuthenticatedAt     *time.Time `json:"lastAuthenticatedAt"`
	PendingCancellationDate *string    `json:"pendingCancellationDate"`
}

type SeatDay struct {
	Day                 string `json:"day"`
	Total               int    `json:"total"`
	Active28d           int    `json:"active28d"`
	NeverActive         int    `json:"neverActive"`
	PendingCancellation int    `json:"pendingCancellation"`
}

type IdentityEmail struct {
	Email  string `json:"email"`
	Source string `json:"source"`
	Org    string `json:"org"`
}

type Identity struct {
	UserID       int64           `json:"userId"`
	Login        string          `json:"login"`
	Name         *string         `json:"name"`
	PrimaryEmail *string         `json:"primaryEmail"`
	Orgs         []string        `json:"orgs"`
	Emails       []IdentityEmail `json:"emails"`
	Seated       bool            `json:"seated"`
}

// Page bounds a listing. Limit must be positive.
type Page struct {
	Limit  int
	Offset int
}

type SeatFilter struct {
	Scope github.Scope
	Login string
	// InactiveSince keeps seats with no activity at or after this time,
	// including seats never used. Zero disables the filter.
	InactiveSince time.Time
	Page          Page
}

type IdentityFilter struct {
	UserID int64
	Login  string
	// Email must already be normalized.
	Email string
	// Q matches login, name or any email as a case-insensitive substring.
	Q    string
	Page Page
}

type jsonCol struct{ dst any }

func (j jsonCol) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		return nil
	case []byte:
		return json.Unmarshal(v, j.dst)
	case string:
		return json.Unmarshal([]byte(v), j.dst)
	default:
		return fmt.Errorf("jsonCol: unsupported type %T", src)
	}
}

// Scopes returns every scope, including inactive ones.
func (s *Store) Scopes(ctx context.Context) ([]ScopeInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, name, installation_id IS NOT NULL, active, copilot_enabled, first_seen_at, last_seen_at
		 FROM scopes ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopeInfo{}
	for rows.Next() {
		var sc ScopeInfo
		if err := rows.Scan(&sc.Kind, &sc.Name, &sc.Installed, &sc.Active, &sc.CopilotEnabled,
			&sc.FirstSeenAt, &sc.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// CollectorStatuses returns each collector's latest run and latest ok or
// partial finish, for every collector that has run at least once.
func (s *Store) CollectorStatuses(ctx context.Context) ([]CollectorStatus, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ON (collector) collector, id, status, started_at, finished_at, items, error
		 FROM collector_runs ORDER BY collector, started_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CollectorStatus{}
	for rows.Next() {
		var st CollectorStatus
		var r Run
		if err := rows.Scan(&st.Collector, &r.ID, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Items, &r.Error); err != nil {
			return nil, err
		}
		st.LastRun = &r
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ok, err := s.LastSuccess(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if t, found := ok[out[i].Collector]; found {
			out[i].LastOkAt = &t
		}
	}
	return out, nil
}

const seatSelect = `SELECT s.scope_kind || ':' || s.scope_name, s.user_id, s.login, u.name, u.primary_email,
	s.org_name, s.assigning_team_slug, s.plan_type, s.created_at, s.updated_at, s.last_activity_at,
	s.last_activity_editor, s.last_authenticated_at, s.pending_cancellation_date::text, count(*) OVER ()
	FROM copilot_seats_current s LEFT JOIN github_users u ON u.user_id = s.user_id`

func (s *Store) querySeats(ctx context.Context, q string, args ...any) ([]Seat, int, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Seat{}
	total := 0
	for rows.Next() {
		var st Seat
		if err := rows.Scan(&st.Scope, &st.UserID, &st.Login, &st.Name, &st.Email, &st.OrgName,
			&st.AssigningTeamSlug, &st.PlanType, &st.CreatedAt, &st.UpdatedAt, &st.LastActivityAt,
			&st.LastActivityEditor, &st.LastAuthenticatedAt, &st.PendingCancellationDate, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, st)
	}
	return out, total, rows.Err()
}

// Seats lists a scope's current seats ordered by login, with the total match
// count before paging.
func (s *Store) Seats(ctx context.Context, f SeatFilter) ([]Seat, int, error) {
	var inactive *time.Time
	if !f.InactiveSince.IsZero() {
		inactive = &f.InactiveSince
	}
	return s.querySeats(ctx, seatSelect+`
		WHERE s.scope_kind = $1 AND s.scope_name = $2
		  AND ($3 = '' OR lower(s.login) = lower($3))
		  AND ($4::timestamptz IS NULL OR s.last_activity_at IS NULL OR s.last_activity_at < $4)
		ORDER BY lower(s.login), s.user_id LIMIT $5 OFFSET $6`,
		string(f.Scope.Kind), f.Scope.Name, f.Login, inactive, f.Page.Limit, f.Page.Offset)
}

// UserSeats returns the user's current seats in every scope.
func (s *Store) UserSeats(ctx context.Context, userID int64) ([]Seat, error) {
	seats, _, err := s.querySeats(ctx, seatSelect+`
		WHERE s.user_id = $1 ORDER BY s.scope_kind, s.scope_name`, userID)
	return seats, err
}

// HasEnterpriseSeats reports whether enterprise seats have been collected.
// Without them the user bundle falls back to org-scope seats.
func (s *Store) HasEnterpriseSeats(ctx context.Context, enterprise string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM copilot_seats_current WHERE scope_kind = 'enterprise' AND scope_name = $1)`,
		enterprise).Scan(&ok)
	return ok, err
}

// SeatHistory aggregates the daily seat snapshots of a scope for from..to
// inclusive. Active28d counts seats with activity in the 28 days before the
// snapshot date (UTC).
func (s *Store) SeatHistory(ctx context.Context, scope github.Scope, from, to time.Time) ([]SeatDay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT snapshot_date::text, count(*),
		   count(*) FILTER (WHERE last_activity_at >= (snapshot_date - 28)::timestamp AT TIME ZONE 'UTC'),
		   count(*) FILTER (WHERE last_activity_at IS NULL),
		   count(*) FILTER (WHERE pending_cancellation_date IS NOT NULL)
		 FROM copilot_seats_snapshot
		 WHERE scope_kind = $1 AND scope_name = $2 AND snapshot_date BETWEEN $3::date AND $4::date
		 GROUP BY snapshot_date ORDER BY snapshot_date`,
		string(scope.Kind), scope.Name, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SeatDay{}
	for rows.Next() {
		var d SeatDay
		if err := rows.Scan(&d.Day, &d.Total, &d.Active28d, &d.NeverActive, &d.PendingCancellation); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Escape ILIKE wildcards in user input before adding the substring pattern.
func likePattern(q string) string {
	if q == "" {
		return ""
	}
	return "%" + likeEscaper.Replace(q) + "%"
}

// Escape ILIKE wildcards in user input before adding the prefix pattern.
func prefixPattern(q string) string {
	if q == "" {
		return ""
	}
	return likeEscaper.Replace(q) + "%"
}

// Identities lists GitHub users with their email mappings, with the total
// match count before paging. Without Q they are ordered by login. With Q an
// exact login comes first, then prefix matches on login, email, or any word of
// the name, then seated users, then login.
func (s *Store) Identities(ctx context.Context, f IdentityFilter) ([]Identity, int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.user_id, u.login, u.name, u.primary_email, to_json(u.orgs),
		   COALESCE((SELECT json_agg(json_build_object('email', e.email, 'source', e.source, 'org', e.org)
		                             ORDER BY e.org, e.email_normalized, e.source)
		             FROM github_user_emails e WHERE e.user_id = u.user_id), '[]'),
		   seated,
		   count(*) OVER ()
		 FROM github_users u
		 CROSS JOIN LATERAL (SELECT EXISTS (SELECT 1 FROM copilot_seats_current s WHERE s.user_id = u.user_id) AS seated) st
		 WHERE ($1 = 0 OR u.user_id = $1)
		   AND ($2 = '' OR lower(u.login) = lower($2))
		   AND ($3 = '' OR EXISTS (SELECT 1 FROM github_user_emails e WHERE e.user_id = u.user_id AND e.email_normalized = $3))
		   AND ($4 = '' OR u.login ILIKE $4 OR u.name ILIKE $4
		        OR EXISTS (SELECT 1 FROM github_user_emails e WHERE e.user_id = u.user_id AND e.email_normalized ILIKE $4))
		 ORDER BY
		   ($7 <> '' AND lower(u.login) = lower($7)) DESC,
		   ($8 <> '' AND (u.login ILIKE $8 OR u.name ILIKE $8 OR u.name ILIKE '% ' || $8
		        OR EXISTS (SELECT 1 FROM github_user_emails e WHERE e.user_id = u.user_id AND e.email_normalized ILIKE $8))) DESC,
		   ($7 <> '' AND seated) DESC,
		   lower(u.login), u.user_id
		 LIMIT $5 OFFSET $6`,
		f.UserID, f.Login, f.Email, likePattern(f.Q), f.Page.Limit, f.Page.Offset, f.Q, prefixPattern(f.Q))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Identity{}
	total := 0
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.UserID, &id.Login, &id.Name, &id.PrimaryEmail, jsonCol{&id.Orgs},
			jsonCol{&id.Emails}, &id.Seated, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, id)
	}
	return out, total, rows.Err()
}

// Identity returns one user by id or login (whichever is set), or nil.
func (s *Store) Identity(ctx context.Context, userID int64, login string) (*Identity, error) {
	ids, _, err := s.Identities(ctx, IdentityFilter{UserID: userID, Login: login, Page: Page{Limit: 1}})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return &ids[0], nil
}

// EmailCandidates returns the users a normalized address maps to, with the
// seat facts identity.Pick orders them by.
func (s *Store) EmailCandidates(ctx context.Context, normalized string) ([]identity.Candidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.user_id, u.login, count(s.user_id) > 0, max(s.last_activity_at)
		 FROM github_users u LEFT JOIN copilot_seats_current s ON s.user_id = u.user_id
		 WHERE u.user_id IN (SELECT user_id FROM github_user_emails WHERE email_normalized = $1)
		 GROUP BY u.user_id, u.login`, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Candidate
	for rows.Next() {
		var c identity.Candidate
		if err := rows.Scan(&c.UserID, &c.Login, &c.Seated, &c.LastActivityAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
