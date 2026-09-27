package github

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

var reportDay = time.Date(2032, 4, 30, 0, 0, 0, 0, time.UTC)

func envelope(t *testing.T, f *fakeGitHub, name, blob string) []byte {
	return []byte(strings.ReplaceAll(string(fixture(t, name)),
		"https://example.blob.core.windows.net/reports/download_links-0?sig=REDACTED", f.blobURL(blob)))
}

func TestReportAPIName(t *testing.T) {
	for _, tc := range []struct {
		t    ReportType
		kind ScopeKind
		w    Window
		want string
	}{
		{ReportSummary, ScopeOrg, Window1Day, "organization-1-day"},
		{ReportSummary, ScopeEnterprise, Window28Day, "enterprise-28-day"},
		{ReportUsers, ScopeEnterprise, Window1Day, "users-1-day"},
		{ReportUserTeams, ScopeOrg, Window1Day, "user-teams-1-day"},
		{ReportRepos, ScopeOrg, Window28Day, "repos-28-day"},
	} {
		if got := tc.t.APIName(tc.kind, tc.w); got != tc.want {
			t.Errorf("%s/%s/%s = %q, want %q", tc.t, tc.kind, tc.w, got, tc.want)
		}
	}
}

func TestDailyReportNDJSON(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /enterprises/team-1/copilot/metrics/reports/users-1-day",
		serveBytes(http.StatusOK, envelope(t, env.fake, "report_envelope_1day.json", "users")))
	env.fake.handle("GET /blob/users", serveBytes(http.StatusOK, fixture(t, "report_users_1day.ndjson")))

	rep, err := env.client.DailyReport(context.Background(), Enterprise(testEnterprise), ReportUsers, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NoData || rep.Day != "2032-04-30" || rep.Name != "users-1-day" || len(rep.Rows) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	if !bytes.Contains(rep.Rows[0], []byte(`"user_login":"user-j"`)) {
		t.Errorf("first row = %.80s", rep.Rows[0])
	}
	if q := env.fake.requests("/enterprises/team-1/copilot/metrics/reports/users-1-day")[0]; q.Query != "day=2032-04-30" || q.Auth != "app:693811" {
		t.Errorf("envelope request = %+v", q)
	}
	if a := env.fake.auths("/blob/users"); !slices.Equal(a, []string{"none"}) {
		t.Errorf("blob download auth = %v, want none", a)
	}
}

func TestLatestReportSingleDocument(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /enterprises/team-1/copilot/metrics/reports/enterprise-28-day/latest",
		serveBytes(http.StatusOK, envelope(t, env.fake, "report_envelope_28day.json", "ent28")))
	env.fake.handle("GET /blob/ent28", serveBytes(http.StatusOK, fixture(t, "report_enterprise_28day.ndjson")))

	rep, err := env.client.LatestReport(context.Background(), Enterprise(testEnterprise), ReportSummary)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StartDay != "2032-04-04" || rep.EndDay != "2032-05-01" || len(rep.Rows) != 1 {
		t.Fatalf("report = %+v rows=%d", rep, len(rep.Rows))
	}
	if !bytes.Contains(rep.Rows[0], []byte(`"day_totals"`)) {
		t.Error("28-day row has no day_totals")
	}
}

func TestDailyReportGzipAndJSONArray(t *testing.T) {
	env := newEnv(t, "")
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`[{"repo_id":1},{"repo_id":2}]`))
	zw.Close()
	env.fake.handle("GET /orgs/org-1/copilot/metrics/reports/repos-1-day",
		serveBytes(http.StatusOK, envelope(t, env.fake, "report_envelope_1day.json", "repos")))
	env.fake.handle("GET /blob/repos", serveBytes(http.StatusOK, gz.Bytes()))

	rep, err := env.client.DailyReport(context.Background(), Org("org-1"), ReportRepos, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 2 || string(rep.Rows[1]) != `{"repo_id":2}` {
		t.Fatalf("rows = %q", rep.Rows)
	}
}

func TestDailyReportNoData(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"204":        serveBytes(http.StatusNoContent, nil),
		"empty json": serveBytes(http.StatusOK, nil), // replaced below by the fixture
		"no links":   serveBytes(http.StatusOK, []byte(`{"download_links":[],"report_day":"2032-04-30"}`)),
	} {
		t.Run(name, func(t *testing.T) {
			env := newEnv(t, "")
			if name == "empty json" {
				h = serveBytes(http.StatusOK, fixture(t, "report_envelope_no_data.json"))
			}
			env.fake.handle("GET /orgs/org-1/copilot/metrics/reports/user-teams-1-day", h)
			rep, err := env.client.DailyReport(context.Background(), Org("org-1"), ReportUserTeams, reportDay)
			if err != nil {
				t.Fatal(err)
			}
			if !rep.NoData || len(rep.Rows) != 0 {
				t.Fatalf("report = %+v, want NoData", rep)
			}
		})
	}
}

func TestExpiredLinkRefetchesEnvelopeOnce(t *testing.T) {
	env := newEnv(t, "")
	envPath := "/orgs/org-1/copilot/metrics/reports/organization-1-day"
	env.fake.handle("GET "+envPath, serveBytes(http.StatusOK, envelope(t, env.fake, "report_envelope_1day.json", "org")))
	env.fake.handle("GET /blob/org", sequence(
		serveBytes(http.StatusForbidden, []byte("AuthenticationFailed")),
		serveBytes(http.StatusOK, fixture(t, "report_organization_1day.ndjson")),
	))
	rep, err := env.client.DailyReport(context.Background(), Org("org-1"), ReportSummary, reportDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %d", len(rep.Rows))
	}
	if n := len(env.fake.requests(envPath)); n != 2 {
		t.Errorf("envelope fetched %d times, want 2", n)
	}
}

func TestExpiredLinkTwiceFailsWithoutLeakingSignature(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/metrics/reports/organization-1-day",
		serveBytes(http.StatusOK, envelope(t, env.fake, "report_envelope_1day.json", "org")))
	env.fake.handle("GET /blob/org", serveBytes(http.StatusForbidden, nil))
	_, err := env.client.DailyReport(context.Background(), Org("org-1"), ReportSummary, reportDay)
	if !errors.Is(err, errLinkExpired) {
		t.Fatalf("err = %v, want errLinkExpired", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the signature: %v", err)
	}
}

func TestDailyReportOutOfRangeDay(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/metrics/reports/users-1-day",
		serveBytes(http.StatusBadRequest, []byte(`{"message":"Date must be within the last year"}`)))
	_, err := env.client.DailyReport(context.Background(), Org("org-1"), ReportUsers, reportDay.AddDate(-2, 0, 0))
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
}

func TestParseRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  int
		isErr bool
	}{
		{"empty", "  \n", 0, false},
		{"array", `[{"a":1},{"a":2}]`, 2, false},
		{"single object", "{\n  \"a\": 1\n}\n", 1, false},
		{"ndjson", "{\"a\":1}\n\n{\"a\":2}\r\n{\"a\":3}\n", 3, false},
		{"bad ndjson line", "{\"a\":1}\n{oops\n", 0, true},
		{"bad array", `[{"a":1},`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseRows([]byte(tc.in))
			if (err != nil) != tc.isErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.isErr)
			}
			if len(rows) != tc.want {
				t.Fatalf("rows = %d, want %d", len(rows), tc.want)
			}
		})
	}
}
