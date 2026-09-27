package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/report"
	"go.dfds.cloud/copilot-insights/internal/store"
)

var (
	ent   = gh.Enterprise("dfds")
	org   = gh.Org("dfds-internal")
	day20 = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	day21 = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	day22 = time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	fixtureDay20 = time.Date(2032, 4, 29, 0, 0, 0, 0, time.UTC)
	fixtureDay21 = time.Date(2032, 4, 30, 0, 0, 0, 0, time.UTC)
	fixtureDay22 = time.Date(2032, 5, 1, 0, 0, 0, 0, time.UTC)
)

func fixtureRows(t *testing.T, name string) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "github", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var rows []json.RawMessage
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		rows = append(rows, json.RawMessage(line))
	}
	return rows
}

func saveReport(t *testing.T, s *store.Store, sc gh.Scope, typ gh.ReportType, w gh.Window, day time.Time, rows []json.RawMessage) {
	t.Helper()
	p, err := report.Parse(typ, w, rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReport(context.Background(), sc, typ.APIName(sc.Kind, w), day, rows, p); err != nil {
		t.Fatalf("save %s %s: %v", typ, w, err)
	}
}

func count(t *testing.T, s *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawRows(lines ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(lines))
	for i, l := range lines {
		out[i] = json.RawMessage(l)
	}
	return out
}

// seedReports stores the fixtures at enterprise scope for 2032-04-30 (users
// also for 2032-04-29), the 28-day engagement, and a small org with teams.
func seedReports(t *testing.T, s *store.Store) {
	t.Helper()
	saveReport(t, s, ent, gh.ReportSummary, gh.Window1Day, fixtureDay21, fixtureRows(t, "report_enterprise_1day.ndjson"))
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, fixtureDay21, fixtureRows(t, "report_users_1day.ndjson"))
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, fixtureDay20, fixtureRows(t, "report_users_1day.ndjson")[:1])
	saveReport(t, s, ent, gh.ReportRepos, gh.Window1Day, fixtureDay21, fixtureRows(t, "report_repos_1day.ndjson"))
	saveReport(t, s, ent, gh.ReportSummary, gh.Window28Day, fixtureDay22, fixtureRows(t, "report_enterprise_28day.ndjson"))

	saveReport(t, s, org, gh.ReportUserTeams, gh.Window1Day, fixtureDay21, fixtureRows(t, "report_user_teams_1day.ndjson"))
	saveReport(t, s, org, gh.ReportUsers, gh.Window1Day, fixtureDay21, rawRows(
		`{"user_id":576713,"user_login":"user-o","used_agent":true,"user_initiated_interaction_count":4,"ai_credits_used":1.5}`,
		`{"user_id":176726,"user_login":"user-p","used_agent":false,"used_chat":true,"user_initiated_interaction_count":6}`,
	))
}

func TestSaveReportReplacesDay(t *testing.T) {
	s, ctx := migrated(t)
	seedReports(t, s)

	users := fixtureRows(t, "report_users_1day.ndjson")
	facts := count(t, s, `SELECT count(*) FROM copilot_user_daily_facts WHERE scope_kind = 'enterprise' AND day = $1`, "2032-04-30")
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, fixtureDay21, users)
	if n := count(t, s, `SELECT count(*) FROM copilot_user_daily WHERE scope_kind = 'enterprise' AND day = $1`, "2032-04-30"); n != 3 {
		t.Errorf("users after re-save = %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM copilot_user_daily_facts WHERE scope_kind = 'enterprise' AND day = $1`, "2032-04-30"); n != facts || n == 0 {
		t.Errorf("facts after re-save = %d, before %d", n, facts)
	}
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, fixtureDay21, users[:1])
	if n := count(t, s, `SELECT count(*) FROM copilot_user_daily WHERE scope_kind = 'enterprise' AND day = $1`, "2032-04-30"); n != 1 {
		t.Errorf("users after shorter re-save = %d", n)
	}

	saveReport(t, s, ent, gh.ReportSummary, gh.Window28Day, fixtureDay21, fixtureRows(t, "report_enterprise_28day.ndjson"))
	saveReport(t, s, ent, gh.ReportSummary, gh.Window1Day, fixtureDay21, fixtureRows(t, "report_enterprise_1day.ndjson"))
	if n := count(t, s, `SELECT count(*) FROM copilot_daily_facts WHERE day = $1 AND breakdown = 'feature_engagement'`, "2032-04-30"); n != 8 {
		t.Errorf("engagement facts after summary re-save = %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM copilot_daily WHERE day = $1`, "2032-04-30"); n != 1 {
		t.Errorf("daily rows = %d", n)
	}

	var status string
	var rowCount int
	var payload []byte
	if err := s.DB().QueryRow(`SELECT status, row_count, payload FROM copilot_report_raw
		WHERE scope_kind = 'enterprise' AND report_type = 'repos-1-day' AND day = '2032-04-30'`).Scan(&status, &rowCount, &payload); err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(payload, &arr); err != nil || status != "complete" || rowCount != 3 || len(arr) != 3 {
		t.Errorf("raw = %s %d %d rows, %v", status, rowCount, len(arr), err)
	}

	if err := s.SaveReportNoData(ctx, ent, "repos-1-day", fixtureDay20); err != nil {
		t.Fatal(err)
	}
	st, err := s.ReportStatuses(ctx, ent, "repos-1-day", fixtureDay20, fixtureDay22)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(st) != "map[2032-04-29:no_data 2032-04-30:complete]" {
		t.Errorf("statuses = %v", st)
	}
}

func TestSaveReportChunksLargeInserts(t *testing.T) {
	s, _ := migrated(t)
	var mcp []string
	for i := range 8000 {
		mcp = append(mcp, fmt.Sprintf(`{"mcp_server":"s%d","call_count":%d}`, i, i+1))
	}
	row := `{"user_id":1,"user_login":"a","totals_by_mcp":[` + strings.Join(mcp, ",") + `]}`
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, fixtureDay21, rawRows(row))
	if n := count(t, s, `SELECT count(*) FROM copilot_user_daily_facts WHERE breakdown = 'mcp'`); n != 8000 {
		t.Errorf("mcp facts = %d, want 8000", n)
	}
}

func TestReportReads(t *testing.T) {
	s, ctx := migrated(t)
	seedReports(t, s)

	days, err := s.DailyReports(ctx, ent, fixtureDay20, fixtureDay22)
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if len(days) != 1 || json.Unmarshal(days[0], &d) != nil || d["day"] != "2032-04-30" ||
		d["daily_active_users"] != float64(292) || d["raw"] != nil || d["scope_name"] != nil {
		t.Errorf("daily = %s", days)
	}
	if _, has := d["weekly_active_vscode_agent_users"]; has {
		t.Error("null column served")
	}
	// Users stored from 2032-04-29 cover no full window yet.
	if _, has := d["rolling_7d_active_users"]; has {
		t.Errorf("partial 7-day window served: %s", days)
	}
	// Stored from 2032-04-23, the 7-day window 04-24..30 is covered and leaves
	// out the 04-23 user; the 28-day window is still partial.
	saveReport(t, s, ent, gh.ReportUsers, gh.Window1Day, time.Date(2032, 4, 23, 0, 0, 0, 0, time.UTC),
		rawRows(`{"user_id":9001,"user_login":"user-early"}`))
	days, _ = s.DailyReports(ctx, ent, fixtureDay20, fixtureDay22)
	if len(days) != 1 || json.Unmarshal(days[0], &d) != nil || d["rolling_7d_active_users"] != float64(3) {
		t.Errorf("daily with 7-day window = %s", days)
	}
	if _, has := d["rolling_28d_active_users"]; has {
		t.Errorf("partial 28-day window served: %s", days)
	}

	facts, err := s.DailyFacts(ctx, ent, fixtureDay20, fixtureDay22, []string{"ide", report.BreakdownFeatureEngagement})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 7 || facts[0].Day != "2032-04-30" || facts[0].Breakdown != "ide" {
		t.Errorf("facts = %+v", facts)
	}

	e, err := s.Engagement(ctx, ent, fixtureDay22)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.PeriodEnd != "2032-05-01" || e.Metrics["active_user_count"] != 228 || len(e.Features) != 7 ||
		e.Features[0].Key1 != "agent_edit" || e.Features[0].Metrics["engaged_user_count"] != 31 {
		t.Errorf("engagement = %+v", e)
	}
	if e, err := s.Engagement(ctx, ent, fixtureDay21); err != nil || e != nil {
		t.Errorf("engagement before period end = %+v, %v", e, err)
	}

	rows, total, err := s.UserDaily(ctx, store.UserDailyFilter{Scope: ent, From: fixtureDay20, To: fixtureDay22, Login: "USER-J", Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 || !strings.Contains(string(rows[0]), `"day": "2032-04-29"`) ||
		strings.Contains(string(rows[0]), `"raw"`) {
		t.Errorf("user daily = %d %s", total, rows)
	}
	if _, total, _ := s.UserDaily(ctx, store.UserDailyFilter{Scope: ent, From: fixtureDay20, To: fixtureDay22, Page: store.Page{Limit: 1}}); total != 4 {
		t.Errorf("user daily total = %d", total)
	}

	uf, err := s.UserFacts(ctx, ent, 659533, fixtureDay20, fixtureDay22, []string{"ide"})
	if err != nil {
		t.Fatal(err)
	}
	if len(uf) != 4 || uf[0].Day != "2032-04-29" {
		t.Errorf("user facts = %+v", uf)
	}
	ft, err := s.UserFactTotals(ctx, ent, 659533, fixtureDay20, fixtureDay22, []string{"ide"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ft) != 2 || ft[0].Day != "" || ft[0].Key1 != "visualstudio" || ft[0].Metrics["user_initiated_interaction_count"] != 518 {
		t.Errorf("user fact totals = %+v", ft)
	}

	ut, total, err := s.UserTotals(ctx, ent, fixtureDay20, fixtureDay22, 0, "", store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(ut) != 3 || ut[0].Login != "user-j" || ut[0].DaysActive != 2 || ut[0].FirstDay != "2032-04-29" ||
		ut[0].LastDay != "2032-04-30" || ut[0].AdoptionPhase == nil || *ut[0].AdoptionPhase != "Phase 1" {
		t.Fatalf("user totals = %+v", ut)
	}
	var tot map[string]any
	if err := json.Unmarshal(ut[0].Totals, &tot); err != nil {
		t.Fatal(err)
	}
	if tot["user_initiated_interaction_count"] != float64(256) || tot["used_agent"] != true || tot["used_cli"] != false ||
		tot["used_copilot_code_review_active"] != false || tot["ai_credits_used"] != 10.8972 {
		t.Errorf("user-j totals = %v", tot)
	}
	if ut, _, _ := s.UserTotals(ctx, ent, fixtureDay20, fixtureDay22, 864404, "", store.Page{Limit: 10}); len(ut) != 1 || ut[0].Login != "user-k" {
		t.Errorf("user totals by id = %+v", ut)
	}

	for repo, want := range map[string]int{"org-3/repo-4": 1, "REPO-3": 1, "": 3, "other/repo-3": 0} {
		if _, total, err := s.RepoDaily(ctx, ent, fixtureDay20, fixtureDay22, repo, store.Page{Limit: 10}); err != nil || total != want {
			t.Errorf("repo %q = %d, %v", repo, total, err)
		}
	}
	rt, total, err := s.RepoTotals(ctx, ent, fixtureDay20, fixtureDay22, store.Page{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rt) != 2 || rt[0].Name != "repo-3" || rt[0].DaysWithData != 1 || !strings.Contains(string(rt[0].Totals), `"pr_total_created": 87`) {
		t.Errorf("repo totals = %d %+v", total, rt)
	}

	teams, err := s.TeamDaily(ctx, org.Name, fixtureDay20, fixtureDay22, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || teams[0].Slug != "team-2" || teams[0].Members != 3 || teams[0].ActiveUsers != 2 {
		t.Fatalf("teams = %+v", teams)
	}
	if err := json.Unmarshal(teams[0].Totals, &tot); err != nil {
		t.Fatal(err)
	}
	if tot["user_initiated_interaction_count"] != float64(10) || tot["used_agent"] != float64(1) || tot["used_chat"] != float64(1) ||
		tot["ai_credits_used"] != 1.5 {
		t.Errorf("team totals = %v", tot)
	}
	if teams, _ := s.TeamDaily(ctx, org.Name, fixtureDay20, fixtureDay22, "other"); len(teams) != 0 {
		t.Errorf("team filter = %+v", teams)
	}

	sum, err := s.Summary(ctx, ent, fixtureDay20, fixtureDay22)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Seats != nil || sum.DaysWithData != 1 || sum.ActiveUsers != 3 || sum.AvgDailyActiveUsers == nil ||
		*sum.AvgDailyActiveUsers != 292 || sum.FeatureEngagement == nil || !strings.Contains(string(sum.LatestDay), `"2032-04-30"`) {
		t.Errorf("summary = %+v", sum)
	}
	if err := json.Unmarshal(sum.Totals, &tot); err != nil {
		t.Fatal(err)
	}
	if tot["user_initiated_interaction_count"] != float64(265) || tot["pr_total_created"] != float64(187) ||
		tot["daily_active_users"] != nil || tot["ai_credits_used"] == nil {
		t.Errorf("summary totals = %v", tot)
	}
	empty, err := s.Summary(ctx, gh.Org("none"), fixtureDay20, fixtureDay22)
	if err != nil || empty.LatestDay != nil || empty.DaysWithData != 0 || empty.FeatureEngagement != nil || string(empty.Totals) != "{}" {
		t.Errorf("empty summary = %+v, %v", empty, err)
	}

	for sc, want := range map[gh.Scope]bool{ent: true, org: true, gh.Org("dfds"): false} {
		if ok, err := s.HasUserReports(ctx, sc); err != nil || ok != want {
			t.Errorf("HasUserReports(%s) = %v, %v", sc, ok, err)
		}
	}
	for id, want := range map[int64]string{576713: "dfds-internal", 659533: ""} {
		if got, err := s.UserReportOrg(ctx, id, fixtureDay20, fixtureDay22); err != nil || got != want {
			t.Errorf("UserReportOrg(%d) = %q, %v", id, got, err)
		}
	}
	if !slices.Equal(queryStrings(t, s, `SELECT DISTINCT scope_name FROM copilot_user_teams`), []string{"dfds-internal"}) {
		t.Error("teams stored under the wrong scope")
	}
}
