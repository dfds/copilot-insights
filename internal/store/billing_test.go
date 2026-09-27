package store_test

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
)

func objects(t *testing.T, raw []json.RawMessage) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		d := json.NewDecoder(strings.NewReader(string(r)))
		d.UseNumber()
		if err := d.Decode(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestBillingUsage(t *testing.T) {
	s, ctx := migrated(t)
	items := []gh.BillingUsageItem{
		{Date: "2026-08-01T00:00:00Z", Product: "copilot", SKU: "Copilot Business", Quantity: 37.2452, UnitType: "UserMonths",
			PricePerUnit: 19, GrossAmount: 707.6588, NetAmount: 707.6588, OrganizationName: "dfds-internal"},
		{Date: "2026-09-01T00:00:00Z", Product: "copilot", SKU: "Copilot AI Credits", Quantity: 1.5, GrossAmount: 1.5, OrganizationName: "dfds"},
		{Date: "2026-09-01T00:00:00Z", Product: "copilot", SKU: "Copilot AI Credits", Quantity: 2.25, GrossAmount: 2.25, OrganizationName: "dfds"},
		{Date: "2026-09-01", Product: "copilot", SKU: "Copilot AI Credits", Quantity: 4, GrossAmount: 4, OrganizationName: "dfds-internal"},
	}
	n, err := s.SaveBillingUsage(ctx, "dfds", items)
	if err != nil || n != 3 {
		t.Fatalf("saved %d, err %v", n, err)
	}
	items[3].Quantity, items[3].GrossAmount = 5, 5
	if _, err := s.SaveBillingUsage(ctx, "dfds", items[3:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBillingUsage(ctx, "dfds", []gh.BillingUsageItem{{Date: "Sept"}}); err == nil {
		t.Error("a malformed date was accepted")
	}

	rows, err := s.BillingUsage(ctx, "dfds", "", store.MonthFilter{Year: 2026})
	if err != nil {
		t.Fatal(err)
	}
	got := objects(t, rows)
	if len(got) != 3 || got[0]["month"] != "2026-08" || got[0]["quantity"].(json.Number) != "37.2452" ||
		got[0]["unitType"] != "UserMonths" || got[1]["quantity"].(json.Number) != "3.75" || got[2]["quantity"].(json.Number) != "5" {
		t.Errorf("usage = %v", got)
	}
	rows, _ = s.BillingUsage(ctx, "dfds", "DFDS", store.MonthFilter{Year: 2026, Month: 9})
	if len(rows) != 1 {
		t.Errorf("org and month filter: %d rows", len(rows))
	}
	rows, _ = s.BillingUsage(ctx, "dfds", "", store.MonthFilter{Year: 2025})
	if len(rows) != 0 {
		t.Errorf("year filter: %d rows", len(rows))
	}

	months, err := s.BillingMonths(ctx, "dfds", "", store.MonthFilter{Year: 2026, Month: 9})
	if err != nil {
		t.Fatal(err)
	}
	m := objects(t, months)
	if len(m) != 1 || m[0]["sku"] != "Copilot AI Credits" || m[0]["quantity"].(json.Number) != "8.75" {
		t.Errorf("months = %v", m)
	}
}

func TestOrgBillingSummaries(t *testing.T) {
	s, ctx := migrated(t)
	for _, c := range []struct {
		org   string
		day   time.Time
		total int
	}{{"dfds", day21, 3}, {"dfds", day22, 4}, {"dfds-internal", day21, 179}} {
		b := &gh.CopilotBilling{PlanType: "business", Policies: map[string]json.RawMessage{"cli": json.RawMessage(`"enabled"`)}}
		b.SeatBreakdown.Total = c.total
		if err := s.SaveCopilotBilling(ctx, c.org, c.day, b); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.OrgBillingSummaries(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got := objects(t, all)
	if len(got) != 2 || got[0]["snapshotDate"] != "2026-09-22" ||
		got[0]["seatBreakdown"].(map[string]any)["total"].(json.Number) != "4" ||
		got[0]["policies"].(map[string]any)["cli"] != "enabled" {
		t.Errorf("summaries = %v", got)
	}
	one, _ := s.OrgBillingSummaries(ctx, "DFDS-internal")
	if len(one) != 1 {
		t.Errorf("org filter: %d rows", len(one))
	}
}

func aiCreditFixture(t *testing.T) []gh.AICreditRow {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "github", "testdata", "ai_credit.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := gh.ParseAICreditCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestAICreditUsage(t *testing.T) {
	s, ctx := migrated(t)
	rows := aiCreditFixture(t)
	users := map[string]bool{}
	for _, r := range rows {
		if r.Date != "2032-04-30" {
			t.Fatalf("fixture row dated %s", r.Date)
		}
		users[r.Username] = true
	}
	// A row repeated within one report is summed into one row.
	dup := rows[0]
	withDup := append(slices.Clone(rows), dup)
	n, err := s.ReplaceAICredits(ctx, "dfds", fixtureDay21, fixtureDay21, withDup, false)
	if err != nil || n != len(rows) {
		t.Fatalf("stored %d of %d, err %v", n, len(rows), err)
	}
	other := gh.AICreditRow{Date: "2032-04-29", Username: rows[0].Username, Product: "copilot", SKU: "s", Model: "m", Quantity: 7}
	if _, err := s.ReplaceAICredits(ctx, "dfds", fixtureDay20, fixtureDay20, []gh.AICreditRow{other}, false); err != nil {
		t.Fatal(err)
	}

	usage, total, err := s.CreditUsage(ctx, store.CreditFilter{Enterprise: "dfds", From: fixtureDay21, To: fixtureDay21,
		GroupBy: []string{"login"}, Page: store.Page{Limit: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(users) || len(usage.Groups) != len(users) {
		t.Errorf("groups %d / %d, want %d users", len(usage.Groups), total, len(users))
	}
	var want float64
	for _, r := range withDup {
		want += r.Quantity
	}
	var tot map[string]float64
	if err := json.Unmarshal(usage.Totals, &tot); err != nil {
		t.Fatal(err)
	}
	if diff := tot["quantity"] - want; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("total quantity %v, want %v", tot["quantity"], want)
	}
	groups := objects(t, usage.Groups)
	first, _ := groups[0]["totals"].(map[string]any)["quantity"].(json.Number).Float64()
	last, _ := groups[len(groups)-1]["totals"].(map[string]any)["quantity"].(json.Number).Float64()
	if first < last {
		t.Error("groups are not ordered by quantity descending")
	}

	// Replacing a day drops rows the new report no longer has, and leaves
	// other days alone.
	if _, err := s.ReplaceAICredits(ctx, "dfds", fixtureDay21, fixtureDay21, rows[:1], false); err != nil {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM ai_credit_usage`); got != 2 {
		t.Errorf("%d rows after replace, want 2", got)
	}

	byDay, total, err := s.CreditUsage(ctx, store.CreditFilter{Enterprise: "dfds", Login: strings.ToUpper(rows[0].Username),
		From: fixtureDay20, To: fixtureDay22, GroupBy: []string{"day", "model"}, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	g := objects(t, byDay.Groups)
	if total != 2 || g[0]["day"] != "2032-04-29" || g[0]["model"] != "m" || g[1]["day"] != "2032-04-30" {
		t.Errorf("by day = %v", g)
	}
	none, total, err := s.CreditUsage(ctx, store.CreditFilter{Enterprise: "dfds", Org: "nope", From: fixtureDay20, To: fixtureDay22})
	if err != nil || total != 0 || len(none.Groups) != 0 || !strings.Contains(string(none.Totals), `"quantity": 0`) {
		t.Errorf("no rows: %s %v %v", none.Totals, total, err)
	}
	if _, _, err := s.CreditUsage(ctx, store.CreditFilter{Enterprise: "dfds", GroupBy: []string{"x"}}); err == nil {
		t.Error("an unknown group was accepted")
	}
	if _, err := s.ReplaceAICredits(ctx, "dfds", fixtureDay21, fixtureDay21, []gh.AICreditRow{{Date: "21-09-2026"}}, false); err == nil {
		t.Error("a malformed date was accepted")
	}

	april := time.Date(2032, 4, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ReplaceAICredits(ctx, "dfds", april, april.AddDate(0, 1, -1), rows, true); err != nil {
		t.Fatal(err)
	}
	settled, err := s.SettledAICreditMonths(ctx, "dfds")
	if err != nil || !settled["2032-04"] || len(settled) != 1 {
		t.Errorf("settled = %v, err %v", settled, err)
	}
	if got := count(t, s, `SELECT count(*) FROM ai_credit_usage`); got != len(rows) {
		t.Errorf("%d rows after the month replace, want %d", got, len(rows))
	}
}

func TestAICreditIncrements(t *testing.T) {
	s, ctx := migrated(t)
	row := func(user, model string, q, gross float64) gh.AICreditRow {
		return gh.AICreditRow{Date: "2026-09-21", Username: user, Product: "copilot", SKU: "s", Model: model,
			Organization: "dfds", Quantity: q, GrossAmount: gross, NetAmount: gross}
	}
	// The first run finds the window empty and records nothing.
	if _, err := s.ReplaceAICredits(ctx, "dfds", day21, day21, []gh.AICreditRow{
		row("alice", "m1", 10, 1), row("bob", "m1", 5, 0.5), row("carol", "m2", 3, 0.3),
	}, false); err != nil {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM ai_credit_increments`); got != 0 {
		t.Fatalf("%d increments after the first run", got)
	}
	// alice grows, bob is revised away, carol is unchanged and dave is new.
	if _, err := s.ReplaceAICredits(ctx, "dfds", day21, day21, []gh.AICreditRow{
		row("alice", "m1", 25, 2.5), row("carol", "m2", 3, 0.3), row("dave", "m2", 40, 4),
	}, false); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	hourly, total, err := s.CreditHourly(ctx, store.CreditFilter{Enterprise: "dfds", From: today, To: today,
		GroupBy: []string{"login"}, Page: store.Page{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, g := range objects(t, hourly.Groups) {
		got[g["login"].(string)] = string(g["totals"].(map[string]any)["quantity"].(json.Number))
		if h, _ := g["hour"].(string); !strings.HasSuffix(h, ":00:00Z") {
			t.Errorf("hour %q", h)
		}
	}
	if want := map[string]string{"alice": "15", "bob": "-5", "dave": "40"}; total != 3 || !maps.Equal(got, want) {
		t.Errorf("increments = %v (%d groups), want %v", got, total, want)
	}
	if !strings.Contains(string(hourly.Totals), `"quantity": 50`) {
		t.Errorf("totals = %s", hourly.Totals)
	}
	top, _, err := s.CreditHourly(ctx, store.CreditFilter{Enterprise: "dfds", From: today, To: today,
		GroupBy: []string{"login"}, Top: 1, Page: store.Page{Limit: 100}})
	if g := objects(t, top.Groups); err != nil || len(g) != 1 || g[0]["login"] != "dave" {
		t.Errorf("top 1 = %v, err %v", g, err)
	}
	none, _, err := s.CreditHourly(ctx, store.CreditFilter{Enterprise: "dfds", From: day20, To: day20})
	if err != nil || len(none.Groups) != 0 || !strings.Contains(string(none.Totals), `"quantity": 0`) {
		t.Errorf("other day: %s %v", none.Totals, err)
	}

	sept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.ReplaceAICredits(ctx, "dfds", sept, sept.AddDate(0, 1, -1), []gh.AICreditRow{row("alice", "m1", 99, 9)}, true); err != nil {
		t.Fatal(err)
	}
	if got := count(t, s, `SELECT count(*) FROM ai_credit_increments`); got != 3 {
		t.Errorf("%d increments after a settle, want 3", got)
	}
}

func TestCreditUsageTop(t *testing.T) {
	s, ctx := migrated(t)
	rows := []gh.AICreditRow{
		{Date: "2026-09-20", Username: "alice", Product: "copilot", SKU: "s", Model: "m1", Quantity: 100, GrossAmount: 1},
		{Date: "2026-09-21", Username: "alice", Product: "copilot", SKU: "s", Model: "m1", Quantity: 100, GrossAmount: 1},
		{Date: "2026-09-21", Username: "bob", Product: "copilot", SKU: "s", Model: "m2", Quantity: 50, GrossAmount: 9},
		{Date: "2026-09-21", Username: "carol", Product: "copilot", SKU: "s", Model: "m2", Quantity: 10, GrossAmount: 5},
	}
	if _, err := s.ReplaceAICredits(ctx, "dfds", day20, day21, rows, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rank    string
		top     int
		include []string
		want    []string
	}{
		{"", 2, nil, []string{"2026-09-20 alice", "2026-09-21 alice", "2026-09-21 bob"}},
		{"gross_amount", 2, nil, []string{"2026-09-21 bob", "2026-09-21 carol"}},
		{"", 1, []string{"Carol", "nobody"}, []string{"2026-09-20 alice", "2026-09-21 alice", "2026-09-21 carol"}},
	} {
		usage, total, err := s.CreditUsage(ctx, store.CreditFilter{Enterprise: "dfds", From: day20, To: day21,
			GroupBy: []string{"day", "login"}, Top: c.top, RankBy: c.rank, Include: c.include, Page: store.Page{Limit: 100}})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, g := range objects(t, usage.Groups) {
			got = append(got, g["day"].(string)+" "+g["login"].(string))
		}
		if total != len(c.want) || !slices.Equal(got, c.want) {
			t.Errorf("rank %q: %v, want %v", c.rank, got, c.want)
		}
	}
	for _, f := range []store.CreditFilter{
		{Enterprise: "dfds", GroupBy: []string{"day"}, Top: 2},
		{Enterprise: "dfds", GroupBy: []string{"login", "model"}, Top: 2},
		{Enterprise: "dfds", GroupBy: []string{"login"}, Top: 2, RankBy: "net_amount"},
		{Enterprise: "dfds", GroupBy: []string{"login"}, Include: []string{"alice"}},
	} {
		if _, _, err := s.CreditUsage(ctx, f); err == nil {
			t.Errorf("%+v was accepted", f)
		}
	}
}

func TestLegacyPRU(t *testing.T) {
	s, ctx := migrated(t)
	seedPeople(t, s)
	item := func(model string, q float64) gh.PremiumRequestUsageItem {
		return gh.PremiumRequestUsageItem{Product: "Copilot", SKU: "Copilot Premium Request", Model: model,
			UnitType: "requests", PricePerUnit: 0.04, GrossQuantity: q, GrossAmount: q * 0.04}
	}
	saves := []struct {
		k     store.LegacyPRUKey
		items []gh.PremiumRequestUsageItem
	}{
		{store.LegacyPRUKey{Year: 2026, Month: 3}, []gh.PremiumRequestUsageItem{item("a", 10), item("b", 5)}},
		{store.LegacyPRUKey{Year: 2026, Month: 3, Org: "dfds-internal"}, []gh.PremiumRequestUsageItem{item("a", 10)}},
		{store.LegacyPRUKey{Year: 2026, Month: 3, Login: "Alice"}, []gh.PremiumRequestUsageItem{item("a", 2), item("a", 1.5)}},
		{store.LegacyPRUKey{Year: 2026, Month: 4, Login: "Alice"}, []gh.PremiumRequestUsageItem{item("a", 9)}},
		{store.LegacyPRUKey{Year: 2026, Month: 4, Login: "bob"}, nil},
	}
	for _, sv := range saves {
		if err := s.SaveLegacyPRU(ctx, "dfds", sv.k, sv.items); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveLegacyPRU(ctx, "dfds", saves[3].k, []gh.PremiumRequestUsageItem{item("c", 4)}); err != nil {
		t.Fatal(err)
	}

	fetched, err := s.LegacyPRUFetched(ctx, "dfds")
	if err != nil || len(fetched) != 5 || !fetched[store.LegacyPRUKey{Year: 2026, Month: 4, Login: "bob"}] {
		t.Errorf("fetched = %v, err %v", fetched, err)
	}

	entRows, err := s.LegacyPRU(ctx, "dfds", "", "", store.MonthFilter{})
	if err != nil || len(entRows) != 2 || objects(t, entRows)[0]["model"] != "a" {
		t.Errorf("enterprise rows = %s, err %v", entRows, err)
	}
	orgRows, _ := s.LegacyPRU(ctx, "dfds", "DFDS-Internal", "", store.MonthFilter{})
	if len(orgRows) != 1 {
		t.Errorf("org rows = %s", orgRows)
	}
	user := objects(t, must(s.LegacyPRU(ctx, "dfds", "", "alice", store.MonthFilter{})))
	if len(user) != 2 || user[0]["month"] != "2026-03" || user[0]["gross_quantity"].(json.Number) != "3.5" ||
		user[1]["month"] != "2026-04" || user[1]["model"] != "c" {
		t.Errorf("user rows = %v", user)
	}
	april, _ := s.LegacyPRU(ctx, "dfds", "", "alice", store.MonthFilter{Year: 2026, Month: 4})
	if len(april) != 1 {
		t.Errorf("month filter: %s", april)
	}

	logins, err := s.UserLogins(ctx)
	if err != nil || !slices.Equal(logins, []string{"alice", "bob", "bob_2", "carol", "dave"}) {
		t.Errorf("logins = %v, err %v", logins, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
