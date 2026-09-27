package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

func newBillingCollectors(g *fakeGitHub, s *fakeStore, now time.Time, backfill, lag int) *Collectors {
	return New(g, s, Config{
		Enterprise:     "dfds",
		Concurrency:    2,
		BackfillDays:   backfill,
		BillingLagDays: lag,
		Now:            func() time.Time { return now },
	}, zap.NewNop())
}

func callsWith(g *fakeGitHub, prefix string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if rest, ok := strings.CutPrefix(c, prefix+" "); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestBilling(t *testing.T) {
	g, s := &fakeGitHub{usage: map[string]*gh.BillingUsageResponse{
		"2026-0": {UsageItems: []gh.BillingUsageItem{
			{Date: "2026-09-01T00:00:00Z", Product: "Copilot", SKU: "Copilot Business"},
			{Date: "2026-09-01T00:00:00Z", Product: "actions", SKU: "Actions Linux"},
		}},
	}}, newFakeStore()
	out, err := newBillingCollectors(g, s, testNow, 90, 0).Billing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := callsWith(g, "usage"); !slices.Equal(got, []string{"2026-0"}) {
		t.Errorf("calls = %v", got)
	}
	if len(s.usage) != 1 || s.usage[0].SKU != "Copilot Business" || out.Items != 1 || out.Scopes != 1 {
		t.Errorf("stored %+v, outcome %+v", s.usage, out)
	}

	g = &fakeGitHub{}
	jan := time.Date(2027, time.January, 5, 3, 0, 0, 0, time.UTC)
	if _, err := newBillingCollectors(g, newFakeStore(), jan, 90, 0).Billing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := callsWith(g, "usage"); !slices.Equal(got, []string{"2027-0", "2026-12"}) {
		t.Errorf("january calls = %v", got)
	}

	g = &fakeGitHub{usageErr: serverError}
	out, _ = newBillingCollectors(g, newFakeStore(), testNow, 90, 0).Billing(context.Background())
	if status, _ := classify(out, nil); status != store.RunError {
		t.Errorf("status = %s, errors %v", status, out.Errors)
	}
	s = newFakeStore()
	s.saveErr = errors.New("db down")
	out, _ = newBillingCollectors(&fakeGitHub{}, s, testNow, 90, 0).Billing(context.Background())
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Error(), "save: db down") {
		t.Errorf("errors = %v", out.Errors)
	}
}

func TestCreditWindows(t *testing.T) {
	at := func(s string) time.Time { return d(s).Add(9 * time.Hour) }
	for _, c := range []struct {
		name     string
		now      time.Time
		backfill int
		lag      int
		settled  []string
		want     []string
	}{
		{"backfill from June", testNow, 90, 0, nil,
			[]string{"2026-06-01..2026-06-30!", "2026-07-01..2026-07-31!", "2026-08-01..2026-08-31!", "2026-09-01..2026-09-23"}},
		{"settled months skipped", testNow, 90, 0, []string{"2026-06", "2026-08"},
			[]string{"2026-07-01..2026-07-31!", "2026-09-01..2026-09-23"}},
		{"backfill bounds the months", testNow, 30, 1, nil,
			[]string{"2026-08-01..2026-08-31!", "2026-09-01..2026-09-22"}},
		{"first of month, lag 0: previous month waits a day", at("2026-10-01"), 0, 0, nil,
			[]string{"2026-10-01..2026-10-01"}},
		{"second of month settles the previous month", at("2026-10-02"), 1, 0, nil,
			[]string{"2026-09-01..2026-09-30!", "2026-10-01..2026-10-02"}},
		{"first of month, lag 1: the month to date is the closed month", at("2026-10-01"), 0, 1, []string{"2026-08"},
			[]string{"2026-09-01..2026-09-30"}},
		{"before AI credits", at("2026-05-10"), 90, 0, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newFakeStore()
			for _, m := range c.settled {
				s.settled[m] = true
			}
			ws, err := newBillingCollectors(nil, s, c.now, c.backfill, c.lag).creditWindows(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range ws {
				k := w.String()
				if w.settle {
					k += "!"
				}
				got = append(got, k)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("windows = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAICredits(t *testing.T) {
	row := []gh.AICreditRow{{Date: "2026-06-03", Username: "u"}}
	g, s := &fakeGitHub{
		aiCredits: map[string][]gh.AICreditRow{
			"2026-06-01..2026-06-30": row,
			"2026-09-01..2026-09-23": row,
		},
		aiCreditErr: map[string]error{"2026-08-01..2026-08-31": serverError},
	}, newFakeStore()
	out, err := newBillingCollectors(g, s, testNow, 90, 0).AICredits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := callsWith(g, "aicredit"); len(got) != 4 {
		t.Errorf("calls = %v", got)
	}
	if !s.settled["2026-06"] || s.settled["2026-07"] || s.settled["2026-08"] {
		t.Errorf("settled = %v", s.settled)
	}
	if s.credits["2026-09-01..2026-09-23"] == nil || s.credits["2026-06-01..2026-06-30!"] == nil {
		t.Errorf("stored = %v", s.credits)
	}
	if out.Scopes != 4 || out.Items != 2 || len(out.Errors) != 2 ||
		!strings.Contains(out.Errors[0].Error(), "2026-07-01..2026-07-31: the report for a closed month has no rows") ||
		!strings.Contains(out.Errors[1].Error(), "2026-08-01..2026-08-31") {
		t.Errorf("outcome = %+v", out)
	}

	// An empty month to date keeps the stored rows and is not an error.
	g, s = &fakeGitHub{}, newFakeStore()
	s.settled["2026-06"], s.settled["2026-07"], s.settled["2026-08"] = true, true, true
	out, _ = newBillingCollectors(g, s, testNow, 90, 0).AICredits(context.Background())
	if len(s.credits) != 0 || len(out.Errors) != 0 || out.Scopes != 1 {
		t.Errorf("empty month to date: stored %v, outcome %+v", s.credits, out)
	}

	s = newFakeStore()
	s.settled["2026-06"], s.settled["2026-07"], s.settled["2026-08"] = true, true, true
	s.saveErr = errors.New("db down")
	out, _ = newBillingCollectors(&fakeGitHub{aiCredits: map[string][]gh.AICreditRow{"2026-09-01..2026-09-23": row}}, s, testNow, 90, 0).
		AICredits(context.Background())
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0].Error(), "save: db down") {
		t.Errorf("save error = %v", out.Errors)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g = &fakeGitHub{aiCreditErr: map[string]error{"2026-06-01..2026-06-30": context.Canceled}}
	out, _ = newBillingCollectors(g, newFakeStore(), testNow, 90, 0).AICredits(ctx)
	if got := callsWith(g, "aicredit"); len(got) != 1 || len(out.Errors) != 1 {
		t.Errorf("after cancel: calls %v, errors %v", got, out.Errors)
	}
}

func TestPremiumLegacy(t *testing.T) {
	item := []gh.PremiumRequestUsageItem{{Product: "Copilot", SKU: "Copilot Premium Request", Model: "m", GrossQuantity: 3}}
	g, s := &fakeGitHub{pru: map[string][]gh.PremiumRequestUsageItem{"2026-3  ": item, "2026-3  u1": item}}, newFakeStore()
	addScope(s, gh.Org("a"), 1, true)
	addScope(s, gh.Org("off"), 2, false)
	addScope(s, gh.Enterprise("dfds"), 3, true)
	s.logins = []string{"u1"}

	out, err := newBillingCollectors(g, s, testNow, 90, 0).PremiumLegacy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := callsWith(g, "pru")
	if len(calls) != 15 || !slices.Contains(calls, "2026-1 a ") || !slices.Contains(calls, "2026-5  u1") ||
		slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "off") }) {
		t.Errorf("calls = %v", calls)
	}
	if len(s.pru) != 15 || len(s.pru[store.LegacyPRUKey{Year: 2026, Month: 3, Login: "u1"}]) != 1 {
		t.Errorf("stored %d requests", len(s.pru))
	}
	if out.Items != 2 || out.Scopes != 15 || len(out.Errors) != 0 {
		t.Errorf("outcome = %+v", out)
	}

	g.calls = nil
	s.logins = append(s.logins, "u2")
	if _, err := newBillingCollectors(g, s, testNow, 90, 0).PremiumLegacy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := callsWith(g, "pru"); len(calls) != 5 || !strings.HasSuffix(calls[0], " u2") {
		t.Errorf("second run calls = %v", calls)
	}
	g.calls = nil
	if out, err := newBillingCollectors(g, s, testNow, 90, 0).PremiumLegacy(context.Background()); err != nil || len(callsWith(g, "pru")) != 0 || out.Scopes != 0 {
		t.Errorf("complete backfill: calls %v, outcome %+v, err %v", g.calls, out, err)
	}
}

func TestPremiumLegacyFailures(t *testing.T) {
	g, s := &fakeGitHub{pruErr: func(q gh.PremiumRequestQuery) error {
		if q.User == "u1" && q.Month == 2 {
			return serverError
		}
		return nil
	}}, newFakeStore()
	addScope(s, gh.Enterprise("dfds"), 3, true)
	s.logins = []string{"u1"}
	out, err := newBillingCollectors(g, s, testNow, 90, 0).PremiumLegacy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status, text := classify(out, nil); status != store.RunPartial ||
		!strings.Contains(text, "1 of 10 requests failed; first: 2026-02 user u1") {
		t.Errorf("status %s: %s", status, text)
	}
	if len(s.pru) != 9 {
		t.Errorf("stored %d requests", len(s.pru))
	}

	// With every request failing, the run gives up early.
	g, s = &fakeGitHub{pruErr: func(gh.PremiumRequestQuery) error { return serverError }}, newFakeStore()
	for i := range 20 {
		s.logins = append(s.logins, fmt.Sprintf("u%d", i))
	}
	_, err = newBillingCollectors(g, s, testNow, 90, 0).PremiumLegacy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "none succeeded") {
		t.Errorf("err = %v", err)
	}
	if n := len(callsWith(g, "pru")); n > legacyPRUGiveUp+2 {
		t.Errorf("%d requests made before giving up", n)
	}
}
