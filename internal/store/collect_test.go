package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.dfds.cloud/copilot-insights/internal/dbtest"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
)

func migrated(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	return store.New(dbtest.OpenMigrated(t)), context.Background()
}

func queryStrings(t *testing.T, s *store.Store, q string, args ...any) []string {
	t.Helper()
	rows, err := s.DB().Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v.String)
	}
	return out
}

func TestSyncScopes(t *testing.T) {
	s, ctx := migrated(t)
	if err := s.SyncScopes(ctx, []store.ScopeState{
		{Scope: gh.Org("dfds"), InstallationID: 1},
		{Scope: gh.Org("old"), InstallationID: 2},
		{Scope: gh.Enterprise("dfds")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCopilotEnabled(ctx, gh.Org("dfds"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncScopes(ctx, []store.ScopeState{
		{Scope: gh.Org("dfds"), InstallationID: 5},
		{Scope: gh.Enterprise("dfds")},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range active {
		got = append(got, fmt.Sprintf("%s#%d enabled=%v", a.Scope, a.InstallationID, a.CopilotEnabled))
	}
	want := []string{"enterprise:dfds#0 enabled=true", "org:dfds#5 enabled=false"}
	if !slices.Equal(got, want) {
		t.Errorf("active = %v, want %v", got, want)
	}
	if n := queryStrings(t, s, `SELECT name FROM scopes WHERE NOT active`); !slices.Equal(n, []string{"old"}) {
		t.Errorf("inactive = %v", n)
	}
}

func member(id int64, login string, emails ...store.Email) store.OrgMember {
	return store.OrgMember{UserID: id, Login: login, Name: "N " + login, Emails: emails}
}

func verified(addr string) store.Email {
	return store.Email{Address: addr, Normalized: addr, Source: store.EmailVerified}
}

func profile(addr string) store.Email {
	return store.Email{Address: addr, Normalized: addr, Source: store.EmailProfile}
}

func TestReplaceOrgMembers(t *testing.T) {
	s, ctx := migrated(t)
	n, err := s.ReplaceOrgMembers(ctx, map[string][]store.OrgMember{
		"a": {member(1, "one", verified("one@dfds.com")), member(2, "two", profile("two@dfds.com"))},
		"b": {member(1, "one", verified("one.b@dfds.com")), member(3, "three", verified("three@dfds.com"))},
	})
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := queryStrings(t, s, `SELECT array_to_string(orgs, ',') FROM github_users WHERE user_id = 1`); !slices.Equal(got, []string{"a,b"}) {
		t.Errorf("orgs of user 1 = %v", got)
	}
	if got := queryStrings(t, s, `SELECT primary_email FROM github_users ORDER BY user_id`); !slices.Equal(got,
		[]string{"one@dfds.com", "two@dfds.com", "three@dfds.com"}) {
		t.Errorf("primary emails = %v", got)
	}

	// Org a is re-enumerated without user 2 and with user 1 renamed; org b is
	// not in the map (its enumeration failed) and keeps its rows.
	if _, err := s.ReplaceOrgMembers(ctx, map[string][]store.OrgMember{
		"a": {member(1, "one-renamed")},
	}); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, s, `SELECT login FROM github_users ORDER BY user_id`); !slices.Equal(got,
		[]string{"one-renamed", "three"}) {
		t.Errorf("users = %v, want user 2 deleted", got)
	}
	if got := queryStrings(t, s, `SELECT org || ':' || email FROM github_user_emails ORDER BY 1`); !slices.Equal(got,
		[]string{"b:one.b@dfds.com", "b:three@dfds.com"}) {
		t.Errorf("emails = %v", got)
	}
	if got := queryStrings(t, s, `SELECT primary_email FROM github_users WHERE user_id = 1`); !slices.Equal(got, []string{"one.b@dfds.com"}) {
		t.Errorf("primary email of user 1 = %v, want the org b address", got)
	}

	if _, err := s.ReplaceOrgMembers(ctx, map[string][]store.OrgMember{"b": {}}); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, s, `SELECT login || '=' || array_to_string(orgs, ',') FROM github_users ORDER BY user_id`); !slices.Equal(got,
		[]string{"one-renamed=a"}) {
		t.Errorf("users = %v", got)
	}
}

func ts(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestSaveSeats(t *testing.T) {
	s, ctx := migrated(t)
	org := gh.Org("dfds")
	d1 := time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	seats := []gh.Seat{
		{UserID: 1, Login: "one", OrgName: "dfds", PlanType: "business", CreatedAt: ts("2025-01-01T00:00:00Z"),
			LastActivityAt: ts("2026-09-22T10:00:00Z"), LastActivityEditor: "vscode/1.2", PendingCancellationDate: "2026-10-01"},
		{UserID: 2, Login: "two", OrgName: "dfds"},
	}
	if err := s.SaveSeats(ctx, org, d1, seats); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSeats(ctx, org, d1, seats[:1]); err != nil { // same day, user 2 removed
		t.Fatal(err)
	}
	seats[0].LastActivityEditor = "jetbrains/3"
	if err := s.SaveSeats(ctx, org, d2, seats[:1]); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, s, `SELECT login || ' ' || last_activity_editor || ' ' || pending_cancellation_date || ' ' || (assigning_team_slug IS NULL)
		FROM copilot_seats_current`); !slices.Equal(got, []string{"one jetbrains/3 2026-10-01 true"}) {
		t.Errorf("current = %v", got)
	}
	if got := queryStrings(t, s, `SELECT snapshot_date::text || ' ' || login FROM copilot_seats_snapshot ORDER BY 1`); !slices.Equal(got,
		[]string{"2026-09-22 one", "2026-09-23 one"}) {
		t.Errorf("snapshots = %v", got)
	}
	if err := s.SaveSeats(ctx, org, d2, nil); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, s, `SELECT login FROM copilot_seats_current`); len(got) != 0 {
		t.Errorf("an empty save left %v", got)
	}
}

func TestSaveCopilotBilling(t *testing.T) {
	s, ctx := migrated(t)
	day := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	b := &gh.CopilotBilling{
		SeatBreakdown: gh.SeatBreakdown{Total: 4, ActiveThisCycle: 3},
		PlanType:      "business",
		Policies:      map[string]json.RawMessage{"cli": json.RawMessage(`"enabled"`)},
	}
	for range 2 {
		if err := s.SaveCopilotBilling(ctx, "dfds", day, b); err != nil {
			t.Fatal(err)
		}
		b.SeatBreakdown.Total = 5
	}
	if got := queryStrings(t, s, `SELECT seat_total || ' ' || seat_active_this_cycle || ' ' || plan_type || ' ' || (policies->>'cli')
		|| ' ' || (seat_management_setting IS NULL) FROM copilot_billing_summary`); !slices.Equal(got, []string{"5 3 business enabled true"}) {
		t.Errorf("billing = %v", got)
	}
}

func TestPruneRawReports(t *testing.T) {
	s, ctx := migrated(t)
	for _, day := range []string{"2025-08-18", "2025-08-19", "2026-09-21"} {
		if _, err := s.DB().Exec(`INSERT INTO copilot_report_raw (scope_kind, scope_name, report_type, day, status)
			VALUES ('org', 'dfds', 'users-1-day', $1, 'no_data')`, day); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneRawReports(ctx, time.Date(2025, 8, 19, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := queryStrings(t, s, `SELECT day::text FROM copilot_report_raw ORDER BY day`); !slices.Equal(got, []string{"2025-08-19", "2026-09-21"}) {
		t.Errorf("left = %v", got)
	}
}
