package store_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
)

func ptr[T any](v T) *T { return &v }

func TestBudgets(t *testing.T) {
	s, ctx := migrated(t)
	budgets := []gh.Budget{
		{ID: "ent", BudgetType: "BundlePricing", BudgetScope: "enterprise", BudgetEntityName: "dfds",
			BudgetProductSKU: "ai_credits", BudgetAmount: 12500, PreventFurtherUsage: true},
		{ID: "multi", BudgetType: "BundlePricing", BudgetScope: gh.BudgetScopeMultiUser, BudgetEntityName: "dfds",
			BudgetProductSKU: "ai_credits", BudgetAmount: 500, PreventFurtherUsage: true},
		{ID: "one", BudgetType: "BundlePricing", BudgetScope: gh.BudgetScopeUser, User: "Alice",
			BudgetProductSKU: "ai_credits", BudgetAmount: 600, ConsumedAmount: ptr(40.5), PreventFurtherUsage: true},
		{ID: "two", BudgetType: "BundlePricing", BudgetScope: gh.BudgetScopeUser, User: "carol",
			BudgetProductSKU: "ai_credits", BudgetAmount: 700, ConsumedAmount: ptr(1.25), PreventFurtherUsage: false},
		{ID: "actions", BudgetType: "ProductPricing", BudgetScope: "enterprise", BudgetEntityName: "dfds",
			BudgetProductSKU: "actions", BudgetAmount: 1000},
	}
	states := map[string][]gh.BudgetUserState{"multi": {
		{User: "alice", ConsumedAmount: 40.25, TargetAmount: 600, OverrideBudgetID: ptr("one")},
		{User: "bob", ConsumedAmount: 12.125, TargetAmount: 500},
	}}
	n, err := s.ReplaceBudgets(ctx, "dfds", budgets, states)
	if err != nil || n != 7 {
		t.Fatalf("saved %d, err %v", n, err)
	}

	userBudget := func(login string) map[string]any {
		t.Helper()
		raw, err := s.UserBudget(ctx, "dfds", login)
		if err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			return nil
		}
		return objects(t, []json.RawMessage{raw})[0]
	}
	cases := []struct {
		login, amount, consumed, scope string
		prevent                        bool
	}{
		// A user state wins and takes the override's scope.
		{"ALICE", "600", "40.25", "user", true},
		{"bob", "500", "12.125", gh.BudgetScopeMultiUser, true},
		// A user budget without a state row.
		{"carol", "700", "1.25", "user", false},
		// No state row: the multi-user amount with nothing consumed.
		{"dave", "500", "0", gh.BudgetScopeMultiUser, true},
	}
	for _, c := range cases {
		got := userBudget(c.login)
		if got == nil || got["amount"].(json.Number).String() != c.amount || got["consumed"].(json.Number).String() != c.consumed ||
			got["scope"] != c.scope || got["preventFurtherUsage"] != c.prevent {
			t.Errorf("%s: budget = %v", c.login, got)
		}
	}

	// ai_credit rows for the enterprise consumed: September to date only.
	rows := []gh.AICreditRow{
		{Date: "2026-08-31", Username: "bob", Product: "copilot", SKU: "copilot_ai_credit", NetAmount: 100},
		{Date: "2026-09-01", Username: "bob", Product: "copilot", SKU: "copilot_ai_credit", NetAmount: 7.5, GrossAmount: 9},
		{Date: "2026-09-23", Username: "alice", Product: "copilot", SKU: "copilot_ai_credit", NetAmount: 2.25, GrossAmount: 3},
	}
	from, to := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	if _, err := s.ReplaceAICredits(ctx, "dfds", from, to, rows, false); err != nil {
		t.Fatal(err)
	}
	sep1 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	list, err := s.Budgets(ctx, "dfds", sep1, to, true)
	if err != nil {
		t.Fatal(err)
	}
	got := objects(t, list)
	byID := map[string]map[string]any{}
	for _, b := range got {
		byID[b["id"].(string)] = b
	}
	if len(got) != 5 {
		t.Fatalf("budgets = %v", got)
	}
	if b := byID["ent"]; b["consumed"].(json.Number) != "9.75" || b["consumedSource"] != "ai_credit_usage" || b["amount"].(json.Number) != "12500" {
		t.Errorf("enterprise budget = %v", b)
	}
	if b := byID["one"]; b["consumed"].(json.Number) != "40.5" || b["consumedSource"] != "github" || b["login"] != "Alice" {
		t.Errorf("user budget = %v", b)
	}
	if b := byID["multi"]; b["consumed"] != nil || b["userStates"].(json.Number) != "2" {
		t.Errorf("multi-user budget = %v", b)
	}
	if b := byID["actions"]; b["consumed"] != nil || b["consumedSource"] != nil {
		t.Errorf("actions budget = %v", b)
	}

	// A past month: the enterprise spend is that month's; GitHub's figures are dropped.
	aug, err := s.Budgets(ctx, "dfds", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range objects(t, aug) {
		switch b["id"] {
		case "ent":
			if b["consumed"].(json.Number) != "100" {
				t.Errorf("August enterprise budget = %v", b)
			}
		case "one", "multi":
			if b["consumed"] != nil || b["consumedSource"] != nil || b["userStates"] != nil {
				t.Errorf("August %s budget = %v", b["id"], b)
			}
		}
	}

	if _, err := s.ReplaceBudgets(ctx, "dfds", budgets[4:], nil); err != nil {
		t.Fatal(err)
	}
	if b := userBudget("bob"); b != nil {
		t.Errorf("budget after replace = %v", b)
	}
	if list, _ := s.Budgets(ctx, "dfds", sep1, to, true); len(list) != 1 || !strings.Contains(string(list[0]), `"actions"`) {
		t.Errorf("budgets after replace = %s", list)
	}
}
