package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	gh "go.dfds.cloud/copilot-insights/internal/github"
)

func fixture(t *testing.T, name string) []json.RawMessage {
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

func value(t *testing.T, cols []string, vals []any, name string) any {
	t.Helper()
	i := slices.Index(cols, name)
	if i < 0 {
		t.Fatalf("no column %s", name)
	}
	return vals[i]
}

func findFact(facts []Fact, breakdown, key1, key2, metric string) (float64, bool) {
	for _, f := range facts {
		if f.Breakdown == breakdown && f.Key1 == key1 && f.Key2 == key2 && f.Metric == metric {
			return f.Value, true
		}
	}
	return 0, false
}

func TestParseEnterpriseDaily(t *testing.T) {
	p, err := Parse(gh.ReportSummary, gh.Window1Day, fixture(t, "report_enterprise_1day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	d := p.Daily
	if d == nil || len(d.Values) != len(DailyColumns) {
		t.Fatalf("daily = %+v", d)
	}
	for col, want := range map[string]any{
		"daily_active_users":                  int64(292),
		"monthly_active_agent_users":          int64(282),
		"user_initiated_interaction_count":    int64(265),
		"daily_active_vscode_agent_users":     int64(292),
		"pr_total_created":                    int64(187),
		"pr_median_minutes_to_merge":          1860.3235,
		"weekly_active_vscode_agent_users":    nil, // absent from the row
		"pr_total_merged_reviewed_by_copilot": int64(25),
	} {
		if got := value(t, DailyColumns, d.Values, col); got != want {
			t.Errorf("%s = %#v, want %#v", col, got, want)
		}
	}

	for _, c := range []struct {
		breakdown, key1, key2, metric string
		want                          float64
	}{
		{"ide", "vscode", "", "user_initiated_interaction_count", 132},
		{"language_model", "c#", "model-fixture-zmhwvqlzdg", "code_generation_activity_count", 83},
		{"model_feature", "model-fixture-vjhlwecgcy", "chat_panel_agent_mode", "user_initiated_interaction_count", 145},
		{"ai_adoption_phase", "No Cohort", "", "users_in_phase_28d", 61},
		{"cli", "", "", "token_usage.output_tokens_sum", 118990},
		{"vscode_agent", "", "", "total_user_messages", 68},
		{BreakdownPRCommentType, "security", "", "total_copilot_applied_suggestions", 36},
	} {
		if got, ok := findFact(d.Facts, c.breakdown, c.key1, c.key2, c.metric); !ok || got != c.want {
			t.Errorf("fact %s/%s/%s/%s = %v (found %v), want %v", c.breakdown, c.key1, c.key2, c.metric, got, ok, c.want)
		}
	}
	for _, f := range d.Facts {
		if f.Value == 0 {
			t.Errorf("zero fact emitted: %+v", f)
		}
		if f.Breakdown == BreakdownExtra {
			t.Errorf("every enterprise field has a column, got extra %+v", f)
		}
	}
	if _, ok := findFact(d.Facts, BreakdownPRCommentType, "bug", "", "total_copilot_applied_suggestions"); ok {
		t.Error("zero-valued comment_type metric emitted")
	}
	if !json.Valid(d.Raw) {
		t.Error("raw is not the row")
	}
}

func TestParseOrgDailySparse(t *testing.T) {
	p, err := Parse(gh.ReportSummary, gh.Window1Day, fixture(t, "report_organization_1day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if got := value(t, DailyColumns, p.Daily.Values, "monthly_active_vscode_agent_users"); got != nil {
		t.Errorf("missing field = %#v, want nil", got)
	}
	if got := value(t, DailyColumns, p.Daily.Values, "pr_median_minutes_to_merge_copilot_authored"); got != nil {
		t.Errorf("missing PR field = %#v, want nil", got)
	}
	if got := value(t, DailyColumns, p.Daily.Values, "daily_active_users"); got != int64(256) {
		t.Errorf("daily_active_users = %#v", got)
	}
}

func TestParseUsers(t *testing.T) {
	p, err := Parse(gh.ReportUsers, gh.Window1Day, fixture(t, "report_users_1day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Users) != 3 {
		t.Fatalf("users = %d", len(p.Users))
	}
	u := p.Users[0]
	if u.UserID != 659533 || u.Login != "user-j" {
		t.Errorf("user = %d %s", u.UserID, u.Login)
	}
	for col, want := range map[string]any{
		"used_agent":                       true,
		"used_cli":                         false,
		"used_copilot_code_review_active":  nil,
		"ai_credits_used":                  5.4486,
		"ai_adoption_phase":                "Phase 1",
		"ai_adoption_phase_number":         int64(1),
		"user_initiated_interaction_count": int64(128),
	} {
		if got := value(t, UserColumns, u.Values, col); got != want {
			t.Errorf("%s = %#v, want %#v", col, got, want)
		}
	}
	if !slices.Equal(u.IDEs, []string{"visualstudio", "vscode"}) {
		t.Errorf("ides = %v", u.IDEs)
	}
	if len(u.Languages) == 0 || !slices.IsSorted(u.Languages) || len(u.Models) == 0 {
		t.Errorf("languages = %v models = %v", u.Languages, u.Models)
	}
	if _, ok := findFact(u.Facts, "ide", "visualstudio", "", "code_acceptance_activity_count"); !ok {
		t.Error("ide fact missing")
	}
	for _, f := range u.Facts {
		if strings.Contains(f.Metric, "user_id") || strings.HasPrefix(f.Metric, "last_known") {
			t.Errorf("non-metric emitted: %+v", f)
		}
	}
	if got, ok := findFact(p.Users[1].Facts, BreakdownExtra, "", "", "distinct_skill_use_count"); !ok || got != 235 {
		t.Errorf("extra distinct_skill_use_count = %v %v", got, ok)
	}
	if _, ok := findFact(u.Facts, BreakdownExtra, "", "", "distinct_skill_use_count"); ok {
		t.Error("zero extra emitted")
	}
}

func TestParseReposAndTeams(t *testing.T) {
	p, err := Parse(gh.ReportRepos, gh.Window1Day, fixture(t, "report_repos_1day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	r := p.Repos[0]
	if r.RepoID != 863926 || r.Owner != "org-3" || r.Name != "repo-3" || r.Visibility != "INTERNAL" ||
		r.OrganizationID != "organization_id-2" {
		t.Errorf("repo = %+v", r)
	}
	if got := value(t, RepoColumns, r.Values, "pr_median_minutes_to_merge"); got != 164.715 {
		t.Errorf("median = %#v", got)
	}

	p, err = Parse(gh.ReportUserTeams, gh.Window1Day, fixture(t, "report_user_teams_1day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (TeamMember{TeamID: 128584, UserID: 576713, Slug: "team-2", Login: "user-o"}); len(p.Teams) != 3 || p.Teams[0] != want {
		t.Errorf("teams = %+v", p.Teams)
	}
}

func TestParseFeatureEngagement(t *testing.T) {
	p, err := Parse(gh.ReportSummary, gh.Window28Day, fixture(t, "report_enterprise_28day.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if p.EndDay != "2032-05-01" || p.Daily != nil {
		t.Errorf("end = %q daily = %v", p.EndDay, p.Daily)
	}
	if got, ok := findFact(p.Engagement, BreakdownFeatureEngagement, "", "", "active_user_count"); !ok || got != 228 {
		t.Errorf("active_user_count = %v", got)
	}
	if got, ok := findFact(p.Engagement, BreakdownFeatureEngagement, "agent_edit", "", "engaged_user_count"); !ok || got != 31 {
		t.Errorf("agent_edit = %v", got)
	}
	if len(p.Engagement) != 8 {
		t.Errorf("engagement facts = %d, want 8", len(p.Engagement))
	}
}

func TestParseErrors(t *testing.T) {
	two := []json.RawMessage{json.RawMessage(`{"day":"2026-09-21"}`), json.RawMessage(`{"day":"2026-09-21"}`)}
	for name, c := range map[string]struct {
		typ  gh.ReportType
		w    gh.Window
		rows []json.RawMessage
	}{
		"two summary rows":   {gh.ReportSummary, gh.Window1Day, two},
		"user without id":    {gh.ReportUsers, gh.Window1Day, []json.RawMessage{json.RawMessage(`{"user_login":"a"}`)}},
		"user without login": {gh.ReportUsers, gh.Window1Day, []json.RawMessage{json.RawMessage(`{"user_id":1}`)}},
		"fractional repo id": {gh.ReportRepos, gh.Window1Day, []json.RawMessage{json.RawMessage(`{"repo_id":1.5}`)}},
		"team without team":  {gh.ReportUserTeams, gh.Window1Day, []json.RawMessage{json.RawMessage(`{"user_id":1}`)}},
		"not an object":      {gh.ReportRepos, gh.Window1Day, []json.RawMessage{json.RawMessage(`[1]`)}},
		"28-day users":       {gh.ReportUsers, gh.Window28Day, two[:1]},
		"28-day no end day":  {gh.ReportSummary, gh.Window28Day, two[:1]},
		"28-day two docs":    {gh.ReportSummary, gh.Window28Day, two},
		"unknown type":       {gh.ReportType("x"), gh.Window1Day, two[:1]},
	} {
		if _, err := Parse(c.typ, c.w, c.rows); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	p, err := Parse(gh.ReportSummary, gh.Window1Day, nil)
	if err != nil || p.Daily != nil {
		t.Errorf("empty summary = %+v, %v", p, err)
	}
}

func TestColumnSets(t *testing.T) {
	for _, c := range DailySumColumns {
		if strings.HasSuffix(c, "_users") || strings.HasPrefix(c, "pr_median") {
			t.Errorf("non-additive column %s in DailySumColumns", c)
		}
	}
	if !slices.Contains(UserSumColumns, "ai_credits_used") || slices.Contains(UserSumColumns, "ai_adoption_phase_number") ||
		slices.Contains(UserSumColumns, "used_agent") {
		t.Errorf("UserSumColumns = %v", UserSumColumns)
	}
	if len(UserFlagColumns) != 9 || len(RepoSumColumns) != 11 {
		t.Errorf("flags = %d, repo sums = %d", len(UserFlagColumns), len(RepoSumColumns))
	}
}
