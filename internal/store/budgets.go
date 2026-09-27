package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
)

// BudgetSKU is the budget_product_sku of Copilot AI credit budgets.
const BudgetSKU = "ai_credits"

var budgetCols = []string{"enterprise", "budget_id", "budget_type", "scope", "entity_name", "login",
	"product_sku", "amount", "consumed", "prevent_further_usage"}

var budgetStateCols = []string{"enterprise", "budget_id", "login", "consumed", "target", "override_budget_id"}

// ReplaceBudgets replaces the enterprise's budgets and user states in one
// transaction. states is keyed by budget id. It returns the number of budgets
// and user states written.
func (s *Store) ReplaceBudgets(ctx context.Context, enterprise string, budgets []github.Budget, states map[string][]github.BudgetUserState) (int, error) {
	budgetRows := make([][]any, 0, len(budgets))
	for _, b := range budgets {
		var consumed any
		if b.ConsumedAmount != nil {
			consumed = num(*b.ConsumedAmount)
		}
		budgetRows = append(budgetRows, []any{enterprise, b.ID, b.BudgetType, b.BudgetScope,
			nullIfEmpty(b.BudgetEntityName), nullIfEmpty(b.User), nullIfEmpty(b.BudgetProductSKU),
			num(b.BudgetAmount), consumed, b.PreventFurtherUsage})
	}
	var stateRows [][]any
	for id, ss := range states {
		for _, st := range ss {
			var override any
			if st.OverrideBudgetID != nil {
				override = *st.OverrideBudgetID
			}
			stateRows = append(stateRows, []any{enterprise, id, st.User, num(st.ConsumedAmount), num(st.TargetAmount), override})
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, table := range []string{"billing_budget_user_states", "billing_budgets"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE enterprise = $1`, enterprise); err != nil {
			return 0, fmt.Errorf("delete %s: %w", table, err)
		}
	}
	if err := insertRows(ctx, tx, "billing_budgets", budgetCols, budgetRows); err != nil {
		return 0, err
	}
	if err := insertRows(ctx, tx, "billing_budget_user_states", budgetStateCols, stateRows); err != nil {
		return 0, err
	}
	return len(budgetRows) + len(stateRows), tx.Commit()
}

// UserBudget returns the AI credit budget that applies to login this month:
// {amount, consumed, scope, preventFurtherUsage}. In order of preference it
// is the user's row in a multi-user budget's states (which already applies
// any user budget override), the user's own budget, or the multi-user budget
// with nothing consumed, since GitHub lists only users who have spent. It is
// nil when no AI credit budget covers the user.
func (s *Store) UserBudget(ctx context.Context, enterprise, login string) (json.RawMessage, error) {
	var out []byte
	err := s.db.QueryRowContext(ctx,
		`WITH b AS (SELECT * FROM billing_budgets WHERE enterprise = $1 AND product_sku = $3)
		 SELECT obj FROM (
		   SELECT 1 AS rank, jsonb_build_object(
		            'amount', st.target, 'consumed', st.consumed,
		            'scope', CASE WHEN o.budget_id IS NULL THEN m.scope ELSE o.scope END,
		            'preventFurtherUsage', coalesce(o.prevent_further_usage, m.prevent_further_usage)) AS obj
		   FROM billing_budget_user_states st
		   JOIN b m ON m.budget_id = st.budget_id
		   LEFT JOIN b o ON o.budget_id = st.override_budget_id
		   WHERE st.enterprise = $1 AND lower(st.login) = lower($2)
		   UNION ALL
		   SELECT 2, jsonb_build_object('amount', amount, 'consumed', coalesce(consumed, 0), 'scope', scope,
		            'preventFurtherUsage', prevent_further_usage)
		   FROM b WHERE scope = $4 AND lower(login) = lower($2)
		   UNION ALL
		   SELECT 3, jsonb_build_object('amount', amount, 'consumed', 0, 'scope', scope,
		            'preventFurtherUsage', prevent_further_usage)
		   FROM b WHERE scope = $5
		 ) x ORDER BY rank LIMIT 1`,
		enterprise, login, BudgetSKU, github.BudgetScopeUser, github.BudgetScopeMultiUser).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// Budgets lists the enterprise's budgets as stored now, with spend for the
// days from..to of one month. GitHub reports no consumed amount for
// enterprise budgets, so an enterprise AI credit budget's consumed is the net
// amount of the ai_credit rows in from..to. GitHub's per-user consumed
// amounts and user state counts cover the current month only, so they are
// null unless current is set. consumedSource says where consumed came from.
func (s *Store) Budgets(ctx context.Context, enterprise string, from, to time.Time, current bool) ([]json.RawMessage, error) {
	f, t := dates(from, to)
	return queryObjects(ctx, s,
		`SELECT jsonb_build_object(
		   'id', b.budget_id, 'type', b.budget_type, 'scope', b.scope, 'entityName', b.entity_name,
		   'login', b.login, 'productSku', b.product_sku, 'amount', b.amount,
		   'consumed', CASE WHEN ent.net IS NOT NULL THEN ent.net WHEN $5 THEN b.consumed END,
		   'consumedSource', CASE WHEN ent.net IS NOT NULL THEN 'ai_credit_usage'
		                          WHEN $5 AND b.consumed IS NOT NULL THEN 'github' END,
		   'preventFurtherUsage', b.prevent_further_usage,
		   'userStates', CASE WHEN $5 THEN (SELECT count(*) FROM billing_budget_user_states st
		                  WHERE st.enterprise = b.enterprise AND st.budget_id = b.budget_id) END,
		   'fetchedAt', b.fetched_at)
		 FROM billing_budgets b
		 LEFT JOIN LATERAL (
		   SELECT coalesce(sum(net_amount), 0) AS net FROM ai_credit_usage
		   WHERE enterprise = b.enterprise AND usage_date BETWEEN $3 AND $4
		 ) ent ON b.scope = 'enterprise' AND b.product_sku = $2
		 WHERE b.enterprise = $1
		 ORDER BY b.product_sku, b.scope, b.amount DESC, b.budget_id`,
		enterprise, BudgetSKU, f, t, current)
}
