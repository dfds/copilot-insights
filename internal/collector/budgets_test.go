package collector

import (
	"context"
	"errors"
	"testing"

	gh "go.dfds.cloud/copilot-insights/internal/github"
)

func TestBudgets(t *testing.T) {
	budgets := []gh.Budget{
		{ID: "ent", BudgetScope: "enterprise", BudgetProductSKU: "ai_credits", BudgetAmount: 12500},
		{ID: "multi", BudgetScope: gh.BudgetScopeMultiUser, BudgetProductSKU: "ai_credits", BudgetAmount: 500},
		{ID: "one", BudgetScope: gh.BudgetScopeUser, User: "user-a", BudgetProductSKU: "ai_credits", BudgetAmount: 600},
	}
	states := []gh.BudgetUserState{{User: "user-a", ConsumedAmount: 12.5, TargetAmount: 600}, {User: "user-b", ConsumedAmount: 3, TargetAmount: 500}}

	t.Run("stores budgets and the multi-user states", func(t *testing.T) {
		g := &fakeGitHub{budgets: budgets, budgetStates: map[string][]gh.BudgetUserState{"multi": states}}
		s := newFakeStore()
		out, err := newCollectors(t, g, s).Budgets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if out.Items != 5 || len(out.Errors) != 0 {
			t.Errorf("outcome = %+v", out)
		}
		if len(s.budgets) != 3 || len(s.budgetStates) != 1 || len(s.budgetStates["multi"]) != 2 {
			t.Errorf("stored budgets = %v, states = %v", s.budgets, s.budgetStates)
		}
		if g.called("budget states ent") || g.called("budget states one") {
			t.Error("user states requested for a budget that is not multi-user")
		}
	})

	t.Run("a failed user states request keeps the stored snapshot", func(t *testing.T) {
		g := &fakeGitHub{budgets: budgets, budgetStateErr: map[string]error{"multi": notFound}}
		s := newFakeStore()
		if _, err := newCollectors(t, g, s).Budgets(context.Background()); !errors.Is(err, notFound) {
			t.Fatalf("err = %v, want the states error", err)
		}
		if s.budgetSaves != 0 {
			t.Error("budgets saved after a failed request")
		}
	})
}
