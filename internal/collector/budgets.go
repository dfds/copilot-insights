package collector

import (
	"context"
	"fmt"

	gh "go.dfds.cloud/copilot-insights/internal/github"
)

// Budgets replaces the stored enterprise budgets and the user states of each
// multi-user budget. A failed request leaves the previous snapshot in place.
func (c *Collectors) Budgets(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	ent := c.cfg.Enterprise
	budgets, err := c.gh.Budgets(ctx, ent)
	if err != nil {
		return out, err
	}
	states := map[string][]gh.BudgetUserState{}
	for _, b := range budgets {
		if b.BudgetScope != gh.BudgetScopeMultiUser {
			continue
		}
		ss, err := c.gh.BudgetUserStates(ctx, ent, b.ID)
		if err != nil {
			return out, fmt.Errorf("budget %s: %w", b.ID, err)
		}
		states[b.ID] = ss
	}
	out.Scopes = 1
	n, err := c.st.ReplaceBudgets(ctx, ent, budgets, states)
	if err != nil {
		return out, fmt.Errorf("save: %w", err)
	}
	out.Items = n
	return out, nil
}
