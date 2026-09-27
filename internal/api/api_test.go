package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.dfds.cloud/copilot-insights/internal/collector"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
	"go.dfds.cloud/copilot-insights/internal/store"
)

var testNow = time.Date(2026, 9, 23, 14, 30, 0, 0, time.UTC)

type fakeReady struct{ err error }

func (f fakeReady) Ready(context.Context) error { return f.err }

type fakeStore struct {
	err        error
	scopes     []store.ScopeInfo
	statuses   []store.CollectorStatus
	lastOK     map[string]time.Time
	seats      []store.Seat
	seatFilter store.SeatFilter
	history    []store.SeatDay
	histArgs   []string
	identities []store.Identity
	idFilter   store.IdentityFilter
	candidates map[string][]identity.Candidate
	lookups    []string

	calls          []string // store call, scope and arguments per report query
	summary        *store.Summary
	daily          []json.RawMessage
	dailyFacts     []store.FactRow
	engagement     *store.Engagement
	userDaily      map[string][]json.RawMessage // by scope
	udFilter       store.UserDailyFilter
	userFacts      []store.FactRow
	userFactTotals []store.FactRow
	userTotals     []store.UserTotals
	repoDaily      []json.RawMessage
	repoTotals     []store.RepoTotals
	teamDays       []store.TeamDay
	userReports    map[string]bool  // HasUserReports by scope
	reportOrg      map[int64]string // UserReportOrg by user

	billingRows   []json.RawMessage
	orgBilling    []json.RawMessage
	creditUsage   *store.CreditUsage
	creditFilters []store.CreditFilter
	legacy        []json.RawMessage
	budget        json.RawMessage
	budgets       []json.RawMessage
}

func (f *fakeStore) UserBudget(_ context.Context, ent, login string) (json.RawMessage, error) {
	f.call("UserBudget", ent, login)
	return f.budget, f.err
}
func (f *fakeStore) Budgets(_ context.Context, ent string, from, to time.Time, current bool) ([]json.RawMessage, error) {
	f.call("Budgets", ent, from, to, current)
	return f.budgets, f.err
}

func (f *fakeStore) BillingUsage(_ context.Context, ent, org string, m store.MonthFilter) ([]json.RawMessage, error) {
	f.call("BillingUsage", ent, org, m.Year, m.Month)
	return f.billingRows, f.err
}
func (f *fakeStore) BillingMonths(_ context.Context, ent, org string, m store.MonthFilter) ([]json.RawMessage, error) {
	f.call("BillingMonths", ent, org, m.Year, m.Month)
	return f.billingRows, f.err
}
func (f *fakeStore) OrgBillingSummaries(_ context.Context, org string) ([]json.RawMessage, error) {
	f.call("OrgBillingSummaries", org)
	return f.orgBilling, f.err
}
func (f *fakeStore) CreditUsage(_ context.Context, cf store.CreditFilter) (*store.CreditUsage, int, error) {
	f.call("CreditUsage", cf.Enterprise, cf.Org, cf.Login, cf.Model, cf.From, cf.To, cf.GroupBy, cf.Page.Limit, cf.Page.Offset)
	f.creditFilters = append(f.creditFilters, cf)
	u := f.creditUsage
	if u == nil {
		u = &store.CreditUsage{Totals: json.RawMessage(`{"quantity":0}`), Groups: []json.RawMessage{}}
	}
	return u, len(u.Groups), f.err
}

func (f *fakeStore) CreditHourly(_ context.Context, cf store.CreditFilter) (*store.CreditUsage, int, error) {
	f.call("CreditHourly", cf.Enterprise, cf.Org, cf.Login, cf.Model, cf.From, cf.To, cf.GroupBy, cf.Top, cf.RankBy,
		cf.Page.Limit, cf.Page.Offset)
	return &store.CreditUsage{Totals: json.RawMessage(`{"quantity":0}`), Groups: []json.RawMessage{}}, 0, f.err
}
func (f *fakeStore) LegacyPRU(_ context.Context, ent, org, login string, m store.MonthFilter) ([]json.RawMessage, error) {
	f.call("LegacyPRU", ent, org, login, m.Year, m.Month)
	if f.legacy == nil {
		return []json.RawMessage{}, f.err
	}
	return f.legacy, f.err
}

func (f *fakeStore) call(parts ...any) {
	s := make([]string, len(parts))
	for i, p := range parts {
		switch v := p.(type) {
		case time.Time:
			s[i] = v.Format(time.DateOnly)
		default:
			s[i] = strings.Trim(strings.ReplaceAll(fmt.Sprint(v), " ", ","), "[]")
		}
	}
	f.calls = append(f.calls, strings.Join(s, " "))
}

func (f *fakeStore) Summary(_ context.Context, sc gh.Scope, from, to time.Time) (*store.Summary, error) {
	f.call("Summary", sc, from, to)
	return f.summary, f.err
}
func (f *fakeStore) DailyReports(_ context.Context, sc gh.Scope, from, to time.Time) ([]json.RawMessage, error) {
	f.call("DailyReports", sc, from, to)
	return f.daily, f.err
}
func (f *fakeStore) DailyFacts(_ context.Context, sc gh.Scope, from, to time.Time, b []string) ([]store.FactRow, error) {
	f.call("DailyFacts", sc, from, to, b)
	return f.dailyFacts, f.err
}
func (f *fakeStore) Engagement(_ context.Context, sc gh.Scope, to time.Time) (*store.Engagement, error) {
	f.call("Engagement", sc, to)
	return f.engagement, f.err
}
func (f *fakeStore) UserDaily(_ context.Context, uf store.UserDailyFilter) ([]json.RawMessage, int, error) {
	f.udFilter = uf
	rows := f.userDaily[uf.Scope.String()]
	return rows, len(rows), f.err
}
func (f *fakeStore) UserFacts(_ context.Context, sc gh.Scope, id int64, from, to time.Time, b []string) ([]store.FactRow, error) {
	f.call("UserFacts", sc, id, from, to, b)
	return f.userFacts, f.err
}
func (f *fakeStore) UserFactTotals(_ context.Context, sc gh.Scope, id int64, from, to time.Time, b []string) ([]store.FactRow, error) {
	f.call("UserFactTotals", sc, id, from, to, b)
	return f.userFactTotals, f.err
}
func (f *fakeStore) UserTotals(_ context.Context, sc gh.Scope, from, to time.Time, id int64, login string, p store.Page) ([]store.UserTotals, int, error) {
	f.call("UserTotals", sc, from, to, id, login, p.Limit)
	var out []store.UserTotals
	for _, u := range f.userTotals {
		if id == 0 || u.UserID == id {
			out = append(out, u)
		}
	}
	return out, len(out), f.err
}
func (f *fakeStore) RepoDaily(_ context.Context, sc gh.Scope, from, to time.Time, repo string, p store.Page) ([]json.RawMessage, int, error) {
	f.call("RepoDaily", sc, from, to, repo, p.Limit, p.Offset)
	return f.repoDaily, len(f.repoDaily), f.err
}
func (f *fakeStore) RepoTotals(_ context.Context, sc gh.Scope, from, to time.Time, p store.Page) ([]store.RepoTotals, int, error) {
	f.call("RepoTotals", sc, from, to, p.Limit)
	return f.repoTotals, len(f.repoTotals), f.err
}
func (f *fakeStore) TeamDaily(_ context.Context, org string, from, to time.Time, team string) ([]store.TeamDay, error) {
	f.call("TeamDaily", org, from, to, team)
	return f.teamDays, f.err
}
func (f *fakeStore) HasUserReports(_ context.Context, sc gh.Scope) (bool, error) {
	return f.userReports[sc.String()], f.err
}
func (f *fakeStore) UserReportOrg(_ context.Context, id int64, from, to time.Time) (string, error) {
	f.call("UserReportOrg", id, from, to)
	return f.reportOrg[id], f.err
}

func (f *fakeStore) Scopes(context.Context) ([]store.ScopeInfo, error) { return f.scopes, f.err }
func (f *fakeStore) CollectorStatuses(context.Context) ([]store.CollectorStatus, error) {
	return f.statuses, f.err
}
func (f *fakeStore) LastSuccess(context.Context) (map[string]time.Time, error) { return f.lastOK, nil }
func (f *fakeStore) Seats(_ context.Context, sf store.SeatFilter) ([]store.Seat, int, error) {
	f.seatFilter = sf
	return f.seats, len(f.seats), f.err
}
func (f *fakeStore) SeatHistory(_ context.Context, sc gh.Scope, from, to time.Time) ([]store.SeatDay, error) {
	f.histArgs = []string{sc.String(), from.Format(time.DateOnly), to.Format(time.DateOnly)}
	return f.history, f.err
}
func (f *fakeStore) UserSeats(_ context.Context, userID int64) ([]store.Seat, error) {
	var out []store.Seat
	for _, s := range f.seats {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, f.err
}
func (f *fakeStore) HasEnterpriseSeats(_ context.Context, ent string) (bool, error) {
	for _, s := range f.seats {
		if s.Scope == "enterprise:"+ent {
			return true, nil
		}
	}
	return false, f.err
}
func (f *fakeStore) Identities(_ context.Context, idf store.IdentityFilter) ([]store.Identity, int, error) {
	f.idFilter = idf
	return f.identities, len(f.identities), f.err
}
func (f *fakeStore) Identity(_ context.Context, userID int64, login string) (*store.Identity, error) {
	for _, id := range f.identities {
		if (userID != 0 && id.UserID == userID) || (login != "" && strings.EqualFold(id.Login, login)) {
			return &id, f.err
		}
	}
	return nil, f.err
}
func (f *fakeStore) EmailCandidates(_ context.Context, email string) ([]identity.Candidate, error) {
	f.lookups = append(f.lookups, email)
	return f.candidates[email], f.err
}

type fakeCollectors struct {
	names     []string
	busy      bool
	triggered []string
}

func (f *fakeCollectors) Names() []string { return f.names }
func (f *fakeCollectors) Trigger(name string) error {
	if !slices.Contains(f.names, name) {
		return collector.ErrUnknownCollector
	}
	if f.busy {
		return collector.ErrBusy
	}
	f.triggered = append(f.triggered, name)
	return nil
}

func newFakeStore() *fakeStore {
	ok := testNow.Add(-time.Hour)
	return &fakeStore{
		scopes: []store.ScopeInfo{
			{Kind: "enterprise", Name: "dfds", Active: true, CopilotEnabled: true},
			{Kind: "org", Name: "dfds", Installed: true, Active: true, CopilotEnabled: true},
			{Kind: "org", Name: "dfds-internal", Installed: true, Active: true, CopilotEnabled: true},
		},
		lastOK:     map[string]time.Time{"members": ok},
		candidates: map[string][]identity.Candidate{},
	}
}

func newRouter(st *fakeStore, coll *fakeCollectors, authMW, adminMW gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if coll == nil {
		coll = &fakeCollectors{names: []string{"members", "seats", "retention"}}
	}
	Configure(r, Options{
		Ready: fakeReady{}, Store: st, Collectors: coll, Enterprise: "dfds",
		AuthMW: authMW, AdminMW: adminMW, Now: func() time.Time { return testNow },
	})
	return r
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Meta  map[string]any  `json:"meta"`
	Error string          `json:"error"`
}

func do(t *testing.T, r http.Handler, method, path string) (int, envelope) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: body is not JSON: %v: %s", method, path, err, w.Body.String())
	}
	return w.Code, env
}

func TestHealthRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name       string
		path       string
		ready      error
		wantCode   int
		wantStatus string
	}{
		{"healthz ignores readiness", "/healthz", errors.New("down"), http.StatusOK, "ok"},
		{"readyz not ready", "/readyz", errors.New("not ready: no successful collector run yet"), http.StatusServiceUnavailable, "not_ready"},
		{"readyz ready", "/readyz", nil, http.StatusOK, "ready"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := gin.New()
			Configure(r, Options{Ready: fakeReady{c.ready}, Store: newFakeStore(), Collectors: &fakeCollectors{}})

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, c.path, nil))
			if w.Code != c.wantCode {
				t.Fatalf("code: got %d, want %d", w.Code, c.wantCode)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if body["status"] != c.wantStatus {
				t.Errorf("status: got %q, want %q", body["status"], c.wantStatus)
			}
		})
	}
}

func TestMiddlewareGuards(t *testing.T) {
	deny := func(code int) gin.HandlerFunc {
		return func(c *gin.Context) { c.AbortWithStatusJSON(code, gin.H{"error": "denied"}) }
	}
	pass := func(c *gin.Context) { c.Next() }

	r := newRouter(newFakeStore(), nil, deny(http.StatusUnauthorized), pass)
	if code, _ := do(t, r, http.MethodGet, "/api/v1/scopes"); code != http.StatusUnauthorized {
		t.Errorf("api route without auth: %d", code)
	}
	if code, _ := do(t, r, http.MethodGet, "/readyz"); code != http.StatusOK {
		t.Errorf("readyz must stay open: %d", code)
	}

	coll := &fakeCollectors{names: []string{"seats"}}
	r = newRouter(newFakeStore(), coll, pass, deny(http.StatusForbidden))
	if code, _ := do(t, r, http.MethodPost, "/api/v1/admin/collect/seats"); code != http.StatusForbidden {
		t.Errorf("admin route without admin role: %d", code)
	}
	if code, _ := do(t, r, http.MethodGet, "/api/v1/scopes"); code != http.StatusOK {
		t.Errorf("read route must not need the admin role: %d", code)
	}
	if len(coll.triggered) != 0 {
		t.Errorf("a denied trigger ran %v", coll.triggered)
	}
}

func TestScopeParam(t *testing.T) {
	r := newRouter(newFakeStore(), nil, nil, nil)
	cases := []struct {
		query    string
		wantCode int
		wantErr  string
	}{
		{"", http.StatusBadRequest, "scope_required"},
		{"?scope=dfds", http.StatusBadRequest, "invalid_scope"},
		{"?scope=team:dfds", http.StatusBadRequest, "invalid_scope"},
		{"?scope=org:", http.StatusBadRequest, "invalid_scope"},
		{"?scope=org:unknown", http.StatusNotFound, "unknown_scope"},
		{"?scope=org:DFDS-Internal", http.StatusOK, ""},
	}
	for _, path := range []string{"/api/v1/seats", "/api/v1/seats/history"} {
		for _, tc := range cases {
			code, env := do(t, r, http.MethodGet, path+tc.query)
			if code != tc.wantCode || env.Error != tc.wantErr {
				t.Errorf("%s%s: %d %q, want %d %q", path, tc.query, code, env.Error, tc.wantCode, tc.wantErr)
			}
			if code == http.StatusOK && env.Meta["scope"] != "org:dfds-internal" {
				t.Errorf("%s%s: meta.scope = %v", path, tc.query, env.Meta["scope"])
			}
		}
	}
}

func TestDateRange(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)
	base := "/api/v1/seats/history?scope=enterprise:dfds"
	cases := []struct {
		query            string
		wantCode         int
		wantErr          string
		wantFrom, wantTo string
	}{
		{"", http.StatusOK, "", "2026-08-27", "2026-09-23"},
		{"&to=2026-09-01", http.StatusOK, "", "2026-08-05", "2026-09-01"},
		{"&from=2026-09-20&to=2026-09-20", http.StatusOK, "", "2026-09-20", "2026-09-20"},
		{"&from=2025-08-20&to=2026-09-23", http.StatusOK, "", "2025-08-20", "2026-09-23"},
		{"&from=2025-08-19&to=2026-09-23", http.StatusBadRequest, "invalid_range", "", ""},
		{"&from=2026-09-21&to=2026-09-20", http.StatusBadRequest, "invalid_range", "", ""},
		{"&from=21-09-2026", http.StatusBadRequest, "invalid_date", "", ""},
	}
	for _, tc := range cases {
		code, env := do(t, r, http.MethodGet, base+tc.query)
		if code != tc.wantCode || env.Error != tc.wantErr {
			t.Errorf("%s: %d %q, want %d %q", tc.query, code, env.Error, tc.wantCode, tc.wantErr)
			continue
		}
		if code != http.StatusOK {
			continue
		}
		if env.Meta["from"] != tc.wantFrom || env.Meta["to"] != tc.wantTo {
			t.Errorf("%s: meta range %v..%v", tc.query, env.Meta["from"], env.Meta["to"])
		}
		if want := []string{"enterprise:dfds", tc.wantFrom, tc.wantTo}; !slices.Equal(st.histArgs, want) {
			t.Errorf("%s: store got %v", tc.query, st.histArgs)
		}
	}
}

func TestListSeats(t *testing.T) {
	st := newFakeStore()
	st.seats = []store.Seat{{Scope: "org:dfds", UserID: 1, Login: "one"}}
	r := newRouter(st, nil, nil, nil)

	code, env := do(t, r, http.MethodGet, "/api/v1/seats?scope=org:dfds&login=One&inactiveDays=30&limit=10&offset=5")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	f := st.seatFilter
	if f.Scope != gh.Org("dfds") || f.Login != "One" || f.Page != (store.Page{Limit: 10, Offset: 5}) ||
		!f.InactiveSince.Equal(testNow.AddDate(0, 0, -30)) {
		t.Errorf("filter = %+v", f)
	}
	page, _ := env.Meta["page"].(map[string]any)
	if page["total"] != 1.0 || page["limit"] != 10.0 {
		t.Errorf("page meta = %v", env.Meta["page"])
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if v, ok := fresh["seats"]; !ok || v != nil {
		t.Errorf("seats freshness should be present and null: %v", fresh)
	}
	if fresh["members"] != testNow.Add(-time.Hour).Format(time.RFC3339) {
		t.Errorf("members freshness = %v", fresh["members"])
	}
	if env.Meta["generatedAt"] != testNow.Format(time.RFC3339) {
		t.Errorf("generatedAt = %v", env.Meta["generatedAt"])
	}

	if _, env := do(t, r, http.MethodGet, "/api/v1/seats?scope=org:dfds"); st.seatFilter.Page.Limit != 500 ||
		!st.seatFilter.InactiveSince.IsZero() || env.Meta["page"] == nil {
		t.Errorf("defaults: %+v", st.seatFilter)
	}
	for _, q := range []string{"inactiveDays=0", "inactiveDays=x", "limit=0", "limit=5001", "offset=-1"} {
		if code, _ := do(t, r, http.MethodGet, "/api/v1/seats?scope=org:dfds&"+q); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
}

func TestStoreErrorIs500(t *testing.T) {
	st := newFakeStore()
	st.err = errors.New("connection refused to 10.0.0.1")
	r := newRouter(st, nil, nil, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "10.0.0.1") {
		t.Errorf("got %d %s", w.Code, w.Body.String())
	}
}

func str(s string) *string { return &s }

func bundleFixture() *fakeStore {
	st := newFakeStore()
	st.identities = []store.Identity{
		{UserID: 1, Login: "alice", PrimaryEmail: str("alice@dfds.com"), Orgs: []string{"dfds-internal"}},
		{UserID: 2, Login: "alice-old", Orgs: []string{"dfds"}},
		{UserID: 3, Login: "bob", Orgs: []string{"dfds"}},
	}
	st.seats = []store.Seat{
		{Scope: "enterprise:dfds", UserID: 1, Login: "alice"},
		{Scope: "org:dfds-internal", UserID: 1, Login: "alice"},
		{Scope: "org:dfds", UserID: 3, Login: "bob"},
	}
	st.candidates = map[string][]identity.Candidate{
		"alice@dfds.com": {{UserID: 2, Login: "alice-old"}, {UserID: 1, Login: "alice", Seated: true}},
		"bob@dfds.com":   {{UserID: 3, Login: "bob", Seated: true}},
	}
	st.userReports = map[string]bool{"enterprise:dfds": true}
	st.userDaily = map[string][]json.RawMessage{
		"enterprise:dfds":   {json.RawMessage(`{"day":"2026-09-02","user_id":1}`)},
		"org:dfds-internal": {json.RawMessage(`{"day":"2026-09-02","user_id":1,"org":true}`)},
	}
	st.userTotals = []store.UserTotals{{UserID: 1, Login: "alice", DaysActive: 1, Totals: json.RawMessage(`{"loc_added_sum":5}`)}}
	st.userFactTotals = []store.FactRow{
		{Breakdown: "ide", Key1: "vscode", Metrics: map[string]float64{"loc_added_sum": 5}},
		{Breakdown: "model_feature", Key1: "gpt", Key2: "chat", Metrics: map[string]float64{"loc_added_sum": 5}},
	}
	return st
}

type bundle struct {
	Identity  store.Identity    `json:"identity"`
	Scopes    map[string]string `json:"scopes"`
	Seats     []store.Seat      `json:"seats"`
	Daily     []map[string]any  `json:"daily"`
	Latest28d *userPeriod       `json:"latest28d"`
}

type userPeriod struct {
	From       string                     `json:"from"`
	To         string                     `json:"to"`
	Totals     store.UserTotals           `json:"totals"`
	Breakdowns map[string][]store.FactRow `json:"breakdowns"`
}

func decodeBundle(t *testing.T, env envelope) bundle {
	t.Helper()
	var b bundle
	if err := json.Unmarshal(env.Data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func seatScopes(b bundle) []string {
	var out []string
	for _, s := range b.Seats {
		out = append(out, s.Scope)
	}
	return out
}

func TestUserBundleScope(t *testing.T) {
	st := bundleFixture()
	r := newRouter(st, nil, nil, nil)

	code, env := do(t, r, http.MethodGet, "/api/v1/users/ALICE?from=2026-09-01&to=2026-09-10")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	b := decodeBundle(t, env)
	if b.Identity.Login != "alice" || !slices.Equal(seatScopes(b), []string{"enterprise:dfds"}) {
		t.Errorf("bundle = %+v", b)
	}
	if env.Meta["scope"] != "enterprise:dfds" || env.Meta["from"] != "2026-09-01" {
		t.Errorf("meta = %v", env.Meta)
	}
	if fmt.Sprint(b.Scopes) != "map[credits:enterprise:dfds reports:enterprise:dfds seats:enterprise:dfds]" || len(b.Daily) != 1 || b.Daily[0]["org"] != nil {
		t.Errorf("reports part = %v %v", b.Scopes, b.Daily)
	}
	if f := st.udFilter; f.UserID != 1 || f.From.Format(time.DateOnly) != "2026-09-01" || f.To.Format(time.DateOnly) != "2026-09-10" ||
		f.Page.Limit != 400 {
		t.Errorf("daily filter = %+v", f)
	}
	// latest28d covers the 28 days ending at the range end.
	l := b.Latest28d
	if l == nil || l.From != "2026-08-14" || l.To != "2026-09-10" || l.Totals.DaysActive != 1 ||
		len(l.Breakdowns["ide"]) != 1 || len(l.Breakdowns["model_feature"]) != 1 || l.Breakdowns["cli"] == nil {
		t.Errorf("latest28d = %+v", l)
	}
	if !slices.Contains(st.calls, "UserFactTotals enterprise:dfds 1 2026-08-14 2026-09-10 ide,feature,language_model,model_feature,cli") {
		t.Errorf("calls = %v", st.calls)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["reports"]; !ok {
		t.Errorf("freshness = %v", fresh)
	}

	// Enterprise seats collected, bob has none there: no fallback to his org
	// seat. He has no report rows either.
	_, env = do(t, r, http.MethodGet, "/api/v1/users/bob")
	if b := decodeBundle(t, env); len(b.Seats) != 0 || env.Meta["scope"] != "enterprise:dfds" || b.Latest28d != nil {
		t.Errorf("bob = %+v %v", b, env.Meta["scope"])
	}

	// No enterprise seats (no PAT) and no enterprise reports: seats come from
	// every org, reports from the one org holding the user's rows.
	st.seats = slices.DeleteFunc(st.seats, func(s store.Seat) bool { return s.Scope == "enterprise:dfds" })
	st.userReports = nil
	st.reportOrg = map[int64]string{1: "dfds-internal"}
	_, env = do(t, r, http.MethodGet, "/api/v1/users/alice?from=2026-09-01&to=2026-09-10")
	b = decodeBundle(t, env)
	if !slices.Equal(seatScopes(b), []string{"org:dfds-internal"}) || b.Scopes["seats"] != "org:*" ||
		b.Scopes["reports"] != "org:dfds-internal" || env.Meta["scope"] != nil || len(b.Daily) != 1 || b.Daily[0]["org"] != true {
		t.Errorf("fallback = %+v %v", b, env.Meta["scope"])
	}
	if !slices.Contains(st.calls, "UserReportOrg 1 2026-08-14 2026-09-10") {
		t.Errorf("calls = %v", st.calls)
	}

	st.reportOrg = nil
	_, env = do(t, r, http.MethodGet, "/api/v1/users/alice")
	if b := decodeBundle(t, env); b.Scopes["reports"] != "" || len(b.Daily) != 0 || b.Latest28d != nil || !strings.Contains(string(env.Data), `"daily":[]`) {
		t.Errorf("no reports = %+v", b)
	}

	if code, env := do(t, r, http.MethodGet, "/api/v1/users/nobody"); code != http.StatusNotFound || env.Error != "user_not_found" {
		t.Errorf("unknown login: %d %q", code, env.Error)
	}
}

func TestUserByEmail(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantCode    int
		wantErr     string
		wantLogin   string
		wantMatched string
		wantOthers  []any
		wantLookups []string
	}{
		{"path value", "/api/v1/users/by-email/Alice@DFDS.com", http.StatusOK, "", "alice", "alice@dfds.com",
			[]any{"alice-old"}, []string{"alice@dfds.com"}},
		{"first mapping wins", "/api/v1/users/by-email/nobody@dfds.com?email=bob@dfds.com&email=alice@dfds.com",
			http.StatusOK, "", "bob", "bob@dfds.com", nil, []string{"nobody@dfds.com", "bob@dfds.com"}},
		{"non-DFDS skipped and duplicates dropped", "/api/v1/users/by-email?email=bob@gmail.com&email=BOB@dfds.com&email=bob@dfds.com",
			http.StatusOK, "", "bob", "bob@dfds.com", nil, []string{"bob@dfds.com"}},
		{"not mapped", "/api/v1/users/by-email?email=nobody@dfds.com&email=x@it.dfds.com", http.StatusNotFound, "not_mapped",
			"", "", nil, []string{"nobody@dfds.com", "x@it.dfds.com"}},
		{"no DFDS address", "/api/v1/users/by-email/bob@notdfds.com", http.StatusBadRequest, "no_dfds_email", "", "", nil, nil},
		{"no address", "/api/v1/users/by-email", http.StatusBadRequest, "email_required", "", "", nil, nil},
		{"bad range checked before lookup", "/api/v1/users/by-email/bob@dfds.com?from=x", http.StatusBadRequest, "invalid_date",
			"", "", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := bundleFixture()
			r := newRouter(st, nil, nil, nil)
			code, env := do(t, r, http.MethodGet, tc.path)
			if code != tc.wantCode || env.Error != tc.wantErr {
				t.Fatalf("got %d %q, want %d %q", code, env.Error, tc.wantCode, tc.wantErr)
			}
			if !slices.Equal(st.lookups, tc.wantLookups) {
				t.Errorf("lookups = %v, want %v", st.lookups, tc.wantLookups)
			}
			if code != http.StatusOK {
				return
			}
			if b := decodeBundle(t, env); b.Identity.Login != tc.wantLogin {
				t.Errorf("login = %q", b.Identity.Login)
			}
			others, _ := env.Meta["otherLogins"].([]any)
			if env.Meta["matchedEmail"] != tc.wantMatched || !slices.Equal(others, tc.wantOthers) {
				t.Errorf("meta = %v", env.Meta)
			}
		})
	}
}

func TestListIdentities(t *testing.T) {
	st := bundleFixture()
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/identity/users?email=%20Alice@DFDS.com&login=alice&q=%20ali%20")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	f := st.idFilter
	if f.Email != "alice@dfds.com" || f.Login != "alice" || f.Q != "ali" || f.Page.Limit != 500 {
		t.Errorf("filter = %+v", f)
	}
	var ids []store.Identity
	if err := json.Unmarshal(env.Data, &ids); err != nil || len(ids) != 3 {
		t.Errorf("data = %s", env.Data)
	}
}

func TestStatus(t *testing.T) {
	st := newFakeStore()
	st.statuses = []store.CollectorStatus{
		{Collector: "metrics", LastRun: &store.Run{ID: 1, Status: "error"}},
		{Collector: "seats", LastRun: &store.Run{ID: 7, Status: "ok"}},
	}
	r := newRouter(st, &fakeCollectors{names: []string{"members", "seats"}}, nil, nil)
	_, env := do(t, r, http.MethodGet, "/api/v1/status")
	var got []store.CollectorStatus
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.Collector)
	}
	if !slices.Equal(names, []string{"members", "seats", "metrics"}) || got[0].LastRun != nil || got[1].LastRun.ID != 7 {
		t.Errorf("status = %s", env.Data)
	}
}

func TestTriggerCollector(t *testing.T) {
	coll := &fakeCollectors{names: []string{"members", "seats"}}
	r := newRouter(newFakeStore(), coll, nil, nil)

	if code, _ := do(t, r, http.MethodPost, "/api/v1/admin/collect/seats"); code != http.StatusAccepted {
		t.Errorf("trigger: %d", code)
	}
	if !slices.Equal(coll.triggered, []string{"seats"}) {
		t.Errorf("triggered = %v", coll.triggered)
	}
	if code, env := do(t, r, http.MethodPost, "/api/v1/admin/collect/nope"); code != http.StatusNotFound || env.Error != "unknown_collector" {
		t.Errorf("unknown: %d %q", code, env.Error)
	}
	coll.busy = true
	if code, env := do(t, r, http.MethodPost, "/api/v1/admin/collect/members"); code != http.StatusConflict || env.Error != "busy" {
		t.Errorf("busy: %d %q", code, env.Error)
	}
	if code := httptestCode(r, http.MethodGet, "/api/v1/admin/collect/members"); code != http.StatusNotFound {
		t.Errorf("GET on the trigger route: %d", code)
	}
}

func httptestCode(r http.Handler, method, path string) int {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code
}

var reportRoutes = []string{
	"/api/v1/summary", "/api/v1/reports/organization/daily", "/api/v1/reports/organization/28d",
	"/api/v1/reports/users/daily", "/api/v1/reports/users/totals", "/api/v1/reports/repos/daily",
	"/api/v1/reports/repos/totals", "/api/v1/teams/daily",
}

func TestReportRoutesNeedScope(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)
	for _, p := range reportRoutes {
		if code, env := do(t, r, http.MethodGet, p); code != http.StatusBadRequest || env.Error != "scope_required" {
			t.Errorf("%s: %d %q", p, code, env.Error)
		}
		if code, env := do(t, r, http.MethodGet, p+"?scope=org:unknown"); code != http.StatusNotFound || env.Error != "unknown_scope" {
			t.Errorf("%s unknown scope: %d %q", p, code, env.Error)
		}
		if code, env := do(t, r, http.MethodGet, p+"?scope=org:dfds&from=2026-09-10&to=2026-09-01"); code != http.StatusBadRequest ||
			env.Error != "invalid_range" {
			t.Errorf("%s bad range: %d %q", p, code, env.Error)
		}
		if code, _ := do(t, r, http.MethodGet, p+"?scope=org:dfds"); code != http.StatusOK {
			t.Errorf("%s: %d", p, code)
		}
	}
	if len(st.calls) == 0 {
		t.Fatal("no store calls")
	}

	st.err = errors.New("db down")
	for _, p := range reportRoutes {
		if code, env := do(t, r, http.MethodGet, p+"?scope=org:dfds"); code != http.StatusInternalServerError || env.Error != "internal" {
			t.Errorf("%s store error: %d %q", p, code, env.Error)
		}
	}
}

func TestSummaryRoute(t *testing.T) {
	st := newFakeStore()
	st.summary = &store.Summary{DaysWithData: 2, Totals: json.RawMessage(`{"loc_added_sum":3}`)}
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/summary?scope=enterprise:dfds")
	if code != http.StatusOK || !strings.Contains(string(env.Data), `"daysWithData":2`) {
		t.Fatalf("%d %s", code, env.Data)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	_, hasReports := fresh["reports"]
	_, hasSeats := fresh["seats"]
	if env.Meta["scope"] != "enterprise:dfds" || !hasReports || !hasSeats || env.Meta["from"] != "2026-08-27" {
		t.Errorf("meta = %v", env.Meta)
	}
	if !slices.Equal(st.calls, []string{"Summary enterprise:dfds 2026-08-27 2026-09-23"}) {
		t.Errorf("calls = %v", st.calls)
	}
}

func TestOrganizationDailyBreakdowns(t *testing.T) {
	st := newFakeStore()
	st.daily = []json.RawMessage{
		json.RawMessage(`{"day":"2026-09-01","daily_active_users":3}`),
		json.RawMessage(`{"day":"2026-09-02","daily_active_users":4}`),
	}
	st.dailyFacts = []store.FactRow{
		{Day: "2026-09-01", Breakdown: "ide", Key1: "vscode", Metrics: map[string]float64{"loc_added_sum": 1}},
		{Day: "2026-09-01", Breakdown: "feature", Key1: "chat", Metrics: map[string]float64{"loc_added_sum": 2}},
	}
	r := newRouter(st, nil, nil, nil)

	_, env := do(t, r, http.MethodGet, "/api/v1/reports/organization/daily?scope=org:dfds")
	if strings.Contains(string(env.Data), "breakdowns") || slices.ContainsFunc(st.calls, func(c string) bool { return strings.HasPrefix(c, "DailyFacts") }) {
		t.Errorf("breakdowns without the parameter: %s %v", env.Data, st.calls)
	}

	code, env := do(t, r, http.MethodGet,
		"/api/v1/reports/organization/daily?scope=org:dfds&from=2026-09-01&to=2026-09-02&breakdown=ide,feature&breakdown=ide")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	var rows []struct {
		Day        string                     `json:"day"`
		DAU        int                        `json:"daily_active_users"`
		Breakdowns map[string][]store.FactRow `json:"breakdowns"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].DAU != 3 || len(rows[0].Breakdowns["ide"]) != 1 || rows[0].Breakdowns["ide"][0].Day != "" ||
		len(rows[0].Breakdowns["feature"]) != 1 || rows[1].Breakdowns == nil || len(rows[1].Breakdowns) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	if !slices.Contains(st.calls, "DailyFacts org:dfds 2026-09-01 2026-09-02 ide,feature") {
		t.Errorf("calls = %v", st.calls)
	}

	var many []string
	for i := range 21 {
		many = append(many, fmt.Sprintf("b%d", i))
	}
	for _, q := range []string{"breakdown=IDE", "breakdown=ide-x", "breakdown=" + strings.Join(many, ",")} {
		if code, env := do(t, r, http.MethodGet, "/api/v1/reports/organization/daily?scope=org:dfds&"+q); code != http.StatusBadRequest ||
			env.Error != "invalid_breakdown" {
			t.Errorf("%s: %d %q", q, code, env.Error)
		}
	}
}

func TestOrganization28d(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/reports/organization/28d?scope=org:dfds&to=2026-09-01")
	if code != http.StatusOK || string(env.Data) != "null" || env.Meta["to"] != "2026-09-01" || env.Meta["from"] != nil {
		t.Errorf("%d %s %v", code, env.Data, env.Meta)
	}
	st.engagement = &store.Engagement{PeriodEnd: "2026-08-31", Metrics: map[string]float64{"active_user_count": 5}}
	_, env = do(t, r, http.MethodGet, "/api/v1/reports/organization/28d?scope=org:dfds")
	if !strings.Contains(string(env.Data), `"periodEnd":"2026-08-31"`) || st.calls[len(st.calls)-1] != "Engagement org:dfds 2026-09-23" {
		t.Errorf("%s %v", env.Data, st.calls)
	}
}

func TestUsersRoutes(t *testing.T) {
	st := newFakeStore()
	st.userDaily = map[string][]json.RawMessage{"enterprise:dfds": {
		json.RawMessage(`{"day":"2026-09-01","user_id":7,"login":"alice"}`),
		json.RawMessage(`{"day":"2026-09-02","user_id":7,"login":"alice"}`),
	}}
	st.userFacts = []store.FactRow{{Day: "2026-09-02", Breakdown: "ide", Key1: "vscode", Metrics: map[string]float64{"x": 1}}}
	r := newRouter(st, nil, nil, nil)

	if code, env := do(t, r, http.MethodGet, "/api/v1/reports/users/daily?scope=enterprise:dfds&breakdown=ide"); code != http.StatusBadRequest ||
		env.Error != "login_required" {
		t.Errorf("breakdown without login: %d %q", code, env.Error)
	}

	code, env := do(t, r, http.MethodGet, "/api/v1/reports/users/daily?scope=enterprise:dfds&login=%20Alice%20&breakdown=ide&limit=50&offset=10")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if f := st.udFilter; f.Login != "Alice" || f.Page != (store.Page{Limit: 50, Offset: 10}) || f.Scope != gh.Enterprise("dfds") {
		t.Errorf("filter = %+v", f)
	}
	if !slices.Contains(st.calls, "UserFacts enterprise:dfds 7 2026-08-27 2026-09-23 ide") {
		t.Errorf("calls = %v", st.calls)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || string(rows[0]["breakdowns"]) != "{}" || !strings.Contains(string(rows[1]["breakdowns"]), "vscode") {
		t.Errorf("rows = %s", env.Data)
	}
	if page, _ := env.Meta["page"].(map[string]any); page["total"] != 2.0 || page["offset"] != 10.0 {
		t.Errorf("page = %v", env.Meta["page"])
	}

	st.calls = nil
	st.userDaily = nil
	if code, _ := do(t, r, http.MethodGet, "/api/v1/reports/users/daily?scope=enterprise:dfds&login=nobody&breakdown=ide"); code != http.StatusOK ||
		len(st.calls) != 0 {
		t.Errorf("facts fetched for no rows: %v", st.calls)
	}

	st.userTotals = []store.UserTotals{{UserID: 7, Login: "alice"}}
	code, env = do(t, r, http.MethodGet, "/api/v1/reports/users/totals?scope=enterprise:dfds&login=alice&limit=5")
	if code != http.StatusOK || !strings.Contains(string(env.Data), `"userId":7`) ||
		st.calls[len(st.calls)-1] != "UserTotals enterprise:dfds 2026-08-27 2026-09-23 0 alice 5" {
		t.Errorf("totals: %d %s %v", code, env.Data, st.calls)
	}
}

func TestReposAndTeamsRoutes(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)

	do(t, r, http.MethodGet, "/api/v1/reports/repos/daily?scope=org:dfds&repo=dfds/x&limit=20&offset=40")
	do(t, r, http.MethodGet, "/api/v1/reports/repos/totals?scope=org:dfds&from=2026-09-01&to=2026-09-02")
	do(t, r, http.MethodGet, "/api/v1/teams/daily?scope=org:DFDS-internal&team=platform")
	want := []string{
		"RepoDaily org:dfds 2026-08-27 2026-09-23 dfds/x 20 40",
		"RepoTotals org:dfds 2026-09-01 2026-09-02 500",
		"TeamDaily dfds-internal 2026-08-27 2026-09-23 platform",
	}
	if !slices.Equal(st.calls, want) {
		t.Errorf("calls = %v", st.calls)
	}
	if code, env := do(t, r, http.MethodGet, "/api/v1/teams/daily?scope=enterprise:dfds"); code != http.StatusBadRequest ||
		env.Error != "org_scope_required" {
		t.Errorf("enterprise teams: %d %q", code, env.Error)
	}
}
