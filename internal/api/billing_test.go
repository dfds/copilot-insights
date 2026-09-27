package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.dfds.cloud/copilot-insights/internal/store"
)

var billingRoutes = []string{
	"/api/v1/billing/summary", "/api/v1/billing/usage", "/api/v1/billing/premium-requests", "/api/v1/credits/usage",
	"/api/v1/credits/hourly",
}

func TestBillingRoutesParams(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)
	for _, p := range billingRoutes {
		if code, env := do(t, r, http.MethodGet, p); code != http.StatusBadRequest || env.Error != "scope_required" {
			t.Errorf("%s: %d %q", p, code, env.Error)
		}
		for q, want := range map[string]string{
			"year=26":            "invalid_year",
			"year=2026&month=13": "invalid_month",
		} {
			if code, env := do(t, r, http.MethodGet, p+"?scope=enterprise:dfds&"+q); code != http.StatusBadRequest || env.Error != want {
				t.Errorf("%s?%s: %d %q, want %s", p, q, code, env.Error, want)
			}
		}
		if code, _ := do(t, r, http.MethodGet, p+"?scope=org:dfds"); code != http.StatusOK {
			t.Errorf("%s: %d", p, code)
		}
		// Only the monthly billing routes default the year.
		code, env := do(t, r, http.MethodGet, p+"?scope=enterprise:dfds&month=3")
		if strings.HasPrefix(p, "/api/v1/billing/summary") || strings.HasPrefix(p, "/api/v1/billing/usage") {
			if code != http.StatusOK || env.Meta["from"] != "2026-03-01" {
				t.Errorf("%s month without year: %d %v", p, code, env.Meta)
			}
		} else if code != http.StatusBadRequest || env.Error != "year_required" {
			t.Errorf("%s month without year: %d %q", p, code, env.Error)
		}
	}
	st.err = errors.New("db down")
	for _, p := range billingRoutes {
		if code, env := do(t, r, http.MethodGet, p+"?scope=org:dfds"); code != http.StatusInternalServerError || env.Error != "internal" {
			t.Errorf("%s store error: %d %q", p, code, env.Error)
		}
	}
}

func TestBillingSummaryAndUsage(t *testing.T) {
	st := newFakeStore()
	st.billingRows = []json.RawMessage{json.RawMessage(`{"month":"2026-09","sku":"Copilot AI Credits"}`)}
	st.orgBilling = []json.RawMessage{json.RawMessage(`{"org":"dfds"}`)}
	r := newRouter(st, nil, nil, nil)

	code, env := do(t, r, http.MethodGet, "/api/v1/billing/summary?scope=enterprise:dfds")
	if code != http.StatusOK || string(env.Data) != `{"months":[{"month":"2026-09","sku":"Copilot AI Credits"}],"seats":[{"org":"dfds"}]}` {
		t.Fatalf("%d %s", code, env.Data)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["billing"]; !ok || env.Meta["from"] != "2026-01-01" || env.Meta["to"] != "2026-12-31" || env.Meta["scope"] != "enterprise:dfds" {
		t.Errorf("meta = %v", env.Meta)
	}

	st.calls = nil
	do(t, r, http.MethodGet, "/api/v1/billing/summary?scope=org:DFDS-internal&year=2026&month=2")
	code, env = do(t, r, http.MethodGet, "/api/v1/billing/usage?scope=org:dfds&year=2025&month=12")
	want := []string{
		"BillingMonths dfds dfds-internal 2026 2", "OrgBillingSummaries dfds-internal",
		"BillingUsage dfds dfds 2025 12",
	}
	if code != http.StatusOK || !slices.Equal(st.calls, want) {
		t.Errorf("calls = %v", st.calls)
	}
	if env.Meta["from"] != "2025-12-01" || env.Meta["to"] != "2025-12-31" || env.Meta["scope"] != "org:dfds" {
		t.Errorf("meta = %v", env.Meta)
	}
}

func TestCreditsUsageRoute(t *testing.T) {
	st := newFakeStore()
	st.creditUsage = &store.CreditUsage{
		Totals: json.RawMessage(`{"quantity":3}`),
		Groups: []json.RawMessage{json.RawMessage(`{"login":"alice","totals":{"quantity":3}}`)},
	}
	r := newRouter(st, nil, nil, nil)

	code, env := do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds")
	if code != http.StatusOK || !strings.Contains(string(env.Data), `"groups":[{"login":"alice"`) {
		t.Fatalf("%d %s", code, env.Data)
	}
	page, _ := env.Meta["page"].(map[string]any)
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["aicredit"]; !ok || page["total"] != float64(1) || env.Meta["from"] != "2026-08-27" {
		t.Errorf("meta = %v", env.Meta)
	}

	st.calls = nil
	do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=org:dfds&year=2026&month=6&login=Alice&model=gpt&groupBy=day,model,day&limit=10&offset=5")
	do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&year=2026&groupBy=none")
	_, env = do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&from=2026-09-01&to=2026-09-02&groupBy=costCenter")
	want := []string{
		"CreditUsage dfds dfds Alice gpt 2026-06-01 2026-06-30 day,model 10 5",
		"CreditUsage dfds    2026-01-01 2026-12-31  500 0",
		"CreditUsage dfds    2026-09-01 2026-09-02 costCenter 500 0",
	}
	if !slices.Equal(st.calls, want) {
		t.Errorf("calls = %q", st.calls)
	}
	if env.Meta["from"] != "2026-09-01" {
		t.Errorf("meta = %v", env.Meta)
	}

	_, env = do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&groupBy=none")
	if env.Meta["page"] != nil {
		t.Errorf("totals only has page meta: %v", env.Meta)
	}
	for q, wantErr := range map[string]string{
		"groupBy=user":   "invalid_group",
		"limit=0":        "invalid_limit",
		"from=2026-13-1": "invalid_date",
	} {
		if code, env := do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&"+q); code != http.StatusBadRequest || env.Error != wantErr {
			t.Errorf("%s: %d %q", q, code, env.Error)
		}
	}
}

func TestCreditsTop(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)

	do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&groupBy=day,login&top=10&rankBy=gross_amount")
	if len(st.creditFilters) != 1 || st.creditFilters[0].Top != 10 || st.creditFilters[0].RankBy != "gross_amount" {
		t.Errorf("filters = %+v", st.creditFilters)
	}
	do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&groupBy=day,login&top=10&include=alice,+bob,,alice")
	if len(st.creditFilters) != 2 || !slices.Equal(st.creditFilters[1].Include, []string{"alice", "bob"}) {
		t.Errorf("filters = %+v", st.creditFilters)
	}
	for q, wantErr := range map[string]string{
		"groupBy=login&include=alice":                                            "invalid_include",
		"groupBy=login&top=10&include=a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q,r,s,t,u": "invalid_include",
		"groupBy=day&top=10":                                                     "invalid_top",
		"groupBy=login,model&top=10":                                             "invalid_top",
		"groupBy=login&top=51":                                                   "invalid_top",
		"groupBy=login&top=x":                                                    "invalid_top",
		"groupBy=login&rankBy=net":                                               "invalid_rank",
	} {
		if code, env := do(t, r, http.MethodGet, "/api/v1/credits/usage?scope=enterprise:dfds&"+q); code != http.StatusBadRequest || env.Error != wantErr {
			t.Errorf("%s: %d %q, want %s", q, code, env.Error, wantErr)
		}
	}
}

func TestCreditsHourlyRoute(t *testing.T) {
	st := newFakeStore()
	r := newRouter(st, nil, nil, nil)

	code, env := do(t, r, http.MethodGet, "/api/v1/credits/hourly?scope=enterprise:dfds")
	page, _ := env.Meta["page"].(map[string]any)
	if code != http.StatusOK || page == nil || env.Meta["from"] != "2026-08-27" {
		t.Errorf("%d meta = %v", code, env.Meta)
	}
	st.calls = nil
	do(t, r, http.MethodGet, "/api/v1/credits/hourly?scope=org:dfds&from=2026-09-01&to=2026-09-02&groupBy=login&top=10&limit=5000")
	do(t, r, http.MethodGet, "/api/v1/credits/hourly?scope=enterprise:dfds&year=2026&month=9&groupBy=none&model=gpt")
	want := []string{
		"CreditHourly dfds dfds   2026-09-01 2026-09-02 login 10  5000 0",
		"CreditHourly dfds   gpt 2026-09-01 2026-09-30  0  500 0",
	}
	if !slices.Equal(st.calls, want) {
		t.Errorf("calls = %q", st.calls)
	}
	for q, wantErr := range map[string]string{
		"groupBy=day":         "invalid_group",
		"groupBy=login,model": "invalid_group",
		"top=10":              "invalid_top",
	} {
		if code, env := do(t, r, http.MethodGet, "/api/v1/credits/hourly?scope=enterprise:dfds&"+q); code != http.StatusBadRequest || env.Error != wantErr {
			t.Errorf("%s: %d %q, want %s", q, code, env.Error, wantErr)
		}
	}
}

func TestPremiumRequestsRoute(t *testing.T) {
	st := newFakeStore()
	st.legacy = []json.RawMessage{json.RawMessage(`{"month":"2026-03"}`)}
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/billing/premium-requests?scope=enterprise:dfds&login=alice&year=2026&month=3")
	if code != http.StatusOK || string(env.Data) != `[{"month":"2026-03"}]` {
		t.Fatalf("%d %s", code, env.Data)
	}
	do(t, r, http.MethodGet, "/api/v1/billing/premium-requests?scope=org:dfds")
	if !slices.Equal(st.calls, []string{"LegacyPRU dfds  alice 2026 3", "LegacyPRU dfds dfds  0 0"}) {
		t.Errorf("calls = %q", st.calls)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["premium-legacy"]; !ok || env.Meta["from"] != "2026-03-01" {
		t.Errorf("meta = %v", env.Meta)
	}
	if code, env := do(t, r, http.MethodGet, "/api/v1/billing/premium-requests?scope=org:dfds&login=alice"); code != http.StatusBadRequest ||
		env.Error != "enterprise_scope_required" {
		t.Errorf("org scope with login: %d %q", code, env.Error)
	}
}

func TestUserBundleCredits(t *testing.T) {
	st := bundleFixture()
	st.creditUsage = &store.CreditUsage{
		Totals: json.RawMessage(`{"quantity":3}`),
		Groups: []json.RawMessage{json.RawMessage(`{"day":"2026-09-02","totals":{"quantity":3}}`)},
	}
	st.legacy = []json.RawMessage{json.RawMessage(`{"month":"2026-03","model":"m"}`)}
	st.budget = json.RawMessage(`{"amount":500,"consumed":12.5,"scope":"multi_user_customer","preventFurtherUsage":true}`)
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/users/alice?from=2026-09-01&to=2026-09-10")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	var b struct {
		Credits struct {
			Totals  map[string]float64 `json:"totals"`
			ByDay   []map[string]any   `json:"byDay"`
			ByModel []map[string]any   `json:"byModel"`
		} `json:"credits"`
		PremiumRequests []map[string]any `json:"premiumRequests"`
		Budget          *struct {
			Amount   float64 `json:"amount"`
			Consumed float64 `json:"consumed"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(env.Data, &b); err != nil {
		t.Fatal(err)
	}
	if b.Credits.Totals["quantity"] != 3 || len(b.Credits.ByDay) != 1 || len(b.Credits.ByModel) != 1 || len(b.PremiumRequests) != 1 {
		t.Errorf("credits = %+v", b)
	}
	if len(st.creditFilters) != 2 || !slices.Equal(st.creditFilters[0].GroupBy, []string{"day"}) ||
		!slices.Equal(st.creditFilters[1].GroupBy, []string{"model"}) || st.creditFilters[0].Login != "alice" ||
		st.creditFilters[0].Org != "" || st.creditFilters[0].From.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("credit filters = %+v", st.creditFilters)
	}
	if !slices.Contains(st.calls, "LegacyPRU dfds  alice 0 0") || !slices.Contains(st.calls, "UserBudget dfds alice") {
		t.Errorf("calls = %q", st.calls)
	}
	if b.Budget == nil || b.Budget.Amount != 500 || b.Budget.Consumed != 12.5 {
		t.Errorf("budget = %+v", b.Budget)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["aicredit"]; !ok {
		t.Errorf("freshness = %v", fresh)
	}
	if _, ok := fresh["budgets"]; !ok {
		t.Errorf("freshness = %v", fresh)
	}

	st.budget = nil
	_, env = do(t, r, http.MethodGet, "/api/v1/users/alice")
	if !strings.Contains(string(env.Data), `"budget":null`) {
		t.Errorf("no budget: %s", env.Data)
	}
}

func TestBudgetsRoute(t *testing.T) {
	st := newFakeStore()
	st.budgets = []json.RawMessage{json.RawMessage(`{"id":"b-1","scope":"enterprise","amount":12500}`)}
	r := newRouter(st, nil, nil, nil)
	code, env := do(t, r, http.MethodGet, "/api/v1/budgets")
	if code != http.StatusOK || string(env.Data) != `[{"id":"b-1","scope":"enterprise","amount":12500}]` {
		t.Fatalf("%d %s", code, env.Data)
	}
	fresh, _ := env.Meta["freshness"].(map[string]any)
	if _, ok := fresh["budgets"]; !ok || env.Meta["scope"] != "enterprise:dfds" || env.Meta["from"] != "2026-09-01" || env.Meta["to"] != "2026-09-23" {
		t.Errorf("meta = %v", env.Meta)
	}

	do(t, r, http.MethodGet, "/api/v1/budgets?year=2026&month=9")
	do(t, r, http.MethodGet, "/api/v1/budgets?year=2026&month=8")
	want := []string{
		"Budgets dfds 2026-09-01 2026-09-23 true",
		"Budgets dfds 2026-09-01 2026-09-23 true",
		"Budgets dfds 2026-08-01 2026-08-31 false",
	}
	if !slices.Equal(st.calls, want) {
		t.Errorf("calls = %q", st.calls)
	}
	for q, wantErr := range map[string]string{
		"year=2026":          "month_required",
		"year=2026&month=10": "invalid_month",
		"month=13&year=2026": "invalid_month",
	} {
		if code, env := do(t, r, http.MethodGet, "/api/v1/budgets?"+q); code != http.StatusBadRequest || env.Error != wantErr {
			t.Errorf("%s: %d %q", q, code, env.Error)
		}
	}
}
