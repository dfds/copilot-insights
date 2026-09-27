package github

import (
	"context"
	"fmt"
	"net/url"
)

// BudgetScopeUser and BudgetScopeMultiUser are the budget_scope values of a
// budget for one user and of one amount applied to every user.
const (
	BudgetScopeUser      = "user"
	BudgetScopeMultiUser = "multi_user_customer"
)

// Budget is one enterprise spending budget. Amounts are USD. GitHub reports
// ConsumedAmount only for user budgets; it is nil otherwise.
type Budget struct {
	ID                  string   `json:"id"`
	BudgetType          string   `json:"budget_type"`
	BudgetAmount        float64  `json:"budget_amount"`
	BudgetScope         string   `json:"budget_scope"`
	BudgetEntityName    string   `json:"budget_entity_name"`
	User                string   `json:"user"`
	BudgetProductSKU    string   `json:"budget_product_sku"`
	PreventFurtherUsage bool     `json:"prevent_further_usage"`
	ConsumedAmount      *float64 `json:"consumed_amount"`
}

// BudgetUserState is one user's spend against a multi-user budget in the
// current month. TargetAmount is the user's own budget when
// OverrideBudgetID names one, else the multi-user amount.
type BudgetUserState struct {
	User             string  `json:"user"`
	ConsumedAmount   float64 `json:"consumed_amount"`
	TargetAmount     float64 `json:"target_amount"`
	OverrideBudgetID *string `json:"override_budget_id"`
}

const budgetsPerPage = 100

// Budgets lists every enterprise budget.
func (c *Client) Budgets(ctx context.Context, enterprise string) ([]Budget, error) {
	base := scopeBase(Enterprise(enterprise)) + "/settings/billing/budgets"
	var out []Budget
	for page := 1; ; page++ {
		var resp struct {
			Budgets     []Budget `json:"budgets"`
			HasNextPage bool     `json:"has_next_page"`
		}
		if err := c.getJSON(ctx, Enterprise(enterprise), familyBudgets, pagePath(base, page), &resp); err != nil {
			return nil, fmt.Errorf("budgets: %w", err)
		}
		out = append(out, resp.Budgets...)
		if !resp.HasNextPage || len(resp.Budgets) == 0 {
			return out, nil
		}
	}
}

// BudgetUserStates lists the per-user spend of a multi-user budget.
func (c *Client) BudgetUserStates(ctx context.Context, enterprise, budgetID string) ([]BudgetUserState, error) {
	base := scopeBase(Enterprise(enterprise)) + "/settings/billing/budgets/" + url.PathEscape(budgetID) + "/user-states"
	var out []BudgetUserState
	for page := 1; ; page++ {
		var resp struct {
			UserStates  []BudgetUserState `json:"user_states"`
			HasNextPage bool              `json:"has_next_page"`
		}
		if err := c.getJSON(ctx, Enterprise(enterprise), familyBudgets, pagePath(base, page), &resp); err != nil {
			return nil, fmt.Errorf("budget user states: %w", err)
		}
		out = append(out, resp.UserStates...)
		if !resp.HasNextPage || len(resp.UserStates) == 0 {
			return out, nil
		}
	}
}

func pagePath(base string, page int) string {
	return withQuery(base, url.Values{"per_page": {itoa(budgetsPerPage)}, "page": {itoa(page)}})
}
