package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

var serverError = &gh.StatusError{Method: http.MethodGet, Path: "/x", Status: http.StatusInternalServerError}

func newReportCollectors(g *fakeGitHub, s *fakeStore, backfill, lag int, teams bool) *Collectors {
	return New(g, s, Config{
		Enterprise:    "dfds",
		Concurrency:   2,
		BackfillDays:  backfill,
		ReportLagDays: lag,
		TeamsEnabled:  teams,
		Now:           func() time.Time { return testNow },
	}, zap.NewNop())
}

func addScope(s *fakeStore, sc gh.Scope, installation int64, enabled bool) {
	s.scopes[sc] = store.ScopeState{Scope: sc, InstallationID: installation, Active: true, CopilotEnabled: enabled}
}

func row(s string) *gh.Report {
	return &gh.Report{Rows: []json.RawMessage{json.RawMessage(s)}}
}

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// callsFor returns the days requested for one scope and report type.
func callsFor(g *fakeGitHub, prefix string) []string {
	var out []string
	for _, c := range g.reportCalls() {
		if day, ok := strings.CutPrefix(c, prefix+" "); ok {
			out = append(out, day)
		}
	}
	return out
}

func TestReportsWindowAndSkips(t *testing.T) {
	g, s := &fakeGitHub{
		reports: map[string]*gh.Report{
			"org:a summary 2026-09-22": row(`{"day":"2026-09-22","daily_active_users":3}`),
			"org:a users 2026-09-22":   row(`{"user_id":1,"user_login":"u"}`),
		},
	}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)
	addScope(s, gh.Enterprise("dfds"), 2, true)
	s.reports[reportKey(gh.Org("a"), "organization-1-day", d("2026-09-21"))] = store.ReportComplete
	s.reports[reportKey(gh.Org("a"), "organization-1-day", d("2026-09-20"))] = store.ReportNoData
	s.reports[reportKey(gh.Org("a"), "organization-1-day", d("2026-09-18"))] = store.ReportNoData

	out, err := newReportCollectors(g, s, 5, 1, false).Reports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Newest first; a complete day and an old no_data day are skipped, a
	// recent no_data day is retried.
	if got := callsFor(g, "org:a summary"); !slices.Equal(got, []string{"2026-09-22", "2026-09-20", "2026-09-19"}) {
		t.Errorf("summary days = %v", got)
	}
	all := []string{"2026-09-22", "2026-09-21", "2026-09-20", "2026-09-19", "2026-09-18"}
	for _, p := range []string{"org:a users", "org:a repos", "enterprise:dfds summary", "enterprise:dfds users"} {
		if got := callsFor(g, p); !slices.Equal(got, all) {
			t.Errorf("%s days = %v", p, got)
		}
	}
	if got := callsFor(g, "org:a user-teams"); got != nil {
		t.Errorf("user-teams fetched with teams disabled: %v", got)
	}
	if !g.called("latest org:a summary") || !g.called("latest enterprise:dfds summary") {
		t.Errorf("latest not fetched: %v", g.calls)
	}
	if out.Scopes != 8 || out.Items != 28 || len(out.Errors) != 0 {
		t.Errorf("outcome = %+v", out)
	}
	if p := s.parsed[reportKey(gh.Org("a"), "organization-1-day", d("2026-09-22"))]; p == nil || p.Daily == nil {
		t.Errorf("summary not parsed: %+v", p)
	}
	if p := s.parsed[reportKey(gh.Org("a"), "users-1-day", d("2026-09-22"))]; p == nil || len(p.Users) != 1 {
		t.Errorf("users not parsed: %+v", p)
	}
	if st := s.reports[reportKey(gh.Enterprise("dfds"), "repos-1-day", d("2026-09-18"))]; st != store.ReportNoData {
		t.Errorf("no-data day stored as %q", st)
	}

	g.calls = nil
	if _, err := newReportCollectors(g, s, 5, 1, false).Reports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := callsFor(g, "org:a users"); !slices.Equal(got, []string{"2026-09-21", "2026-09-20"}) {
		t.Errorf("second run users days = %v", got)
	}
}

func TestReportsScopeSelection(t *testing.T) {
	g, s := &fakeGitHub{}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)
	addScope(s, gh.Org("off"), 2, false)
	addScope(s, gh.Enterprise("dfds"), 0, true)

	out, err := newReportCollectors(g, s, 1, 1, true).Reports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"org:a summary 2026-09-22", "org:a users 2026-09-22", "org:a repos 2026-09-22", "org:a user-teams 2026-09-22"}
	got := g.reportCalls()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("calls = %v", got)
	}
	if out.Scopes != 5 || out.Items != 4 {
		t.Errorf("outcome = %+v", out)
	}
}

func TestReportsFailures(t *testing.T) {
	g, s := &fakeGitHub{
		reportErr: map[string]error{
			"org:a users 2026-09-22": serverError,
			"org:a users 2026-09-21": serverError,
			"org:a users 2026-09-20": serverError,
			"org:a repos 2026-09-22": notFound, // recent: not generated yet
			"org:a repos 2026-09-18": notFound,
		},
		reports: map[string]*gh.Report{
			"org:a summary 2026-09-19": {Rows: []json.RawMessage{json.RawMessage(`{}`), json.RawMessage(`{}`)}},
		},
	}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)

	out, err := newReportCollectors(g, s, 5, 1, false).Reports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := callsFor(g, "org:a users"); !slices.Equal(got, []string{"2026-09-22", "2026-09-21", "2026-09-20"}) {
		t.Errorf("users days = %v, want a stop after 3 failures", got)
	}
	msgs := map[string]string{}
	for _, e := range out.Errors {
		scope, msg, _ := strings.Cut(e.Error(), ": ")
		msgs[scope] = msg
	}
	if len(out.Errors) != 3 ||
		!strings.HasPrefix(msgs["org:a users-1-day"], "stopped after 3 consecutive failures; first: 2026-09-22") ||
		!strings.HasPrefix(msgs["org:a repos-1-day"], "1 days failed; first: 2026-09-18") ||
		!strings.Contains(msgs["org:a organization-1-day"], "2026-09-19: parse") {
		t.Errorf("errors = %v", out.Errors)
	}
	if _, stored := s.reports[reportKey(gh.Org("a"), "repos-1-day", d("2026-09-22"))]; stored {
		t.Error("a not-yet-generated day was stored")
	}
	if status, _ := classify(out, nil); status != store.RunPartial {
		t.Errorf("status = %s", status)
	}

	g2, s2 := &fakeGitHub{reports: map[string]*gh.Report{"org:a summary 2026-09-22": row(`{"day":"x"}`)}}, newFakeStore()
	addScope(s2, gh.Org("a"), 1, true)
	s2.saveErr = errors.New("db down")
	out, _ = newReportCollectors(g2, s2, 1, 1, false).Reports(context.Background())
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Error(), "save: db down") {
		t.Errorf("save error = %v", out.Errors)
	}
}

func TestReportsLatest(t *testing.T) {
	g, s := &fakeGitHub{latest: map[string]*gh.Report{
		"org:a summary":           row(`{"report_end_day":"2026-09-22","copilot_feature_engagement":{"active_user_count":5}}`),
		"enterprise:dfds summary": row(`{"report_end_day":"22-09-2026"}`),
	}}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)
	addScope(s, gh.Enterprise("dfds"), 2, true)

	out, err := newReportCollectors(g, s, 1, 0, false).Reports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := s.parsed[reportKey(gh.Org("a"), "organization-28-day", d("2026-09-22"))]
	if p == nil || len(p.Engagement) != 1 || p.Engagement[0].Value != 5 {
		t.Errorf("engagement = %+v", p)
	}
	if len(out.Errors) != 1 || !strings.HasPrefix(out.Errors[0].Error(), "enterprise:dfds enterprise-28-day: report_end_day") {
		t.Errorf("errors = %v", out.Errors)
	}
	if got := callsFor(g, "org:a summary"); !slices.Equal(got, []string{"2026-09-23"}) {
		t.Errorf("days with lag 0 = %v", got)
	}
}

func TestReportWindow(t *testing.T) {
	for _, c := range []struct {
		backfill, lag      int
		first, last, retry string
	}{
		{90, 1, "2026-06-25", "2026-09-22", "2026-09-20"},
		{365, 1, "2025-09-24", "2026-09-22", "2026-09-20"},
		{0, 1, "2026-09-22", "2026-09-22", "2026-09-20"},
	} {
		first, last, retry := newReportCollectors(nil, nil, c.backfill, c.lag, false).reportWindow()
		if got := [3]string{first.Format(time.DateOnly), last.Format(time.DateOnly), retry.Format(time.DateOnly)}; got != [3]string{c.first, c.last, c.retry} {
			t.Errorf("backfill %d lag %d: window = %v", c.backfill, c.lag, got)
		}
	}
}

func TestReportsStopOnCancel(t *testing.T) {
	g, s := &fakeGitHub{}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := newReportCollectors(g, s, 5, 1, false).Reports(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.reportCalls()) != 0 || len(out.Errors) == 0 || !errors.Is(out.Errors[0], context.Canceled) {
		t.Errorf("calls = %v errors = %v", g.reportCalls(), out.Errors)
	}
}
