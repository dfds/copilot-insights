package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
)

// num renders a float for a NUMERIC column. The shortest round-trip form
// keeps the report's decimal text, where a float8 parameter cast to numeric
// could add binary noise.
func num(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// monthStart parses a billing usage date (RFC 3339 or YYYY-MM-DD) to the
// first day of its month.
func monthStart(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse(time.DateOnly, s); err != nil {
			return time.Time{}, fmt.Errorf("date %q is neither RFC 3339 nor YYYY-MM-DD", s)
		}
	}
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
}

func excludedSet(cols ...string) string {
	parts := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		parts = append(parts, c+" = EXCLUDED."+c)
	}
	return strings.Join(append(parts, "fetched_at = now()"), ", ")
}

var billingUsageCols = []string{"enterprise", "month", "product", "sku", "org_name", "repo_name",
	"quantity", "unit_type", "price_per_unit", "gross_amount", "discount_amount", "net_amount"}

// SaveBillingUsage upserts enterprise billing usage items on (month, product,
// sku, org, repository). Items sharing that key are summed. It returns the
// number of rows written.
func (s *Store) SaveBillingUsage(ctx context.Context, enterprise string, items []github.BillingUsageItem) (int, error) {
	type key struct {
		month                   time.Time
		product, sku, org, repo string
	}
	merged := map[key]*github.BillingUsageItem{}
	var order []key
	for _, it := range items {
		m, err := monthStart(it.Date)
		if err != nil {
			return 0, err
		}
		k := key{m, it.Product, it.SKU, it.OrganizationName, it.RepositoryName}
		if cur, ok := merged[k]; ok {
			cur.Quantity += it.Quantity
			cur.GrossAmount += it.GrossAmount
			cur.DiscountAmount += it.DiscountAmount
			cur.NetAmount += it.NetAmount
			continue
		}
		merged[k] = &it
		order = append(order, k)
	}
	rows := make([][]any, 0, len(order))
	for _, k := range order {
		it := merged[k]
		rows = append(rows, []any{enterprise, k.month.Format(time.DateOnly), k.product, k.sku, k.org, k.repo,
			num(it.Quantity), nullIfEmpty(it.UnitType), num(it.PricePerUnit), num(it.GrossAmount),
			num(it.DiscountAmount), num(it.NetAmount)})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	err = writeRows(ctx, tx, "billing_usage", billingUsageCols, rows,
		"ON CONFLICT (enterprise, month, product, sku, org_name, repo_name) DO UPDATE SET "+
			excludedSet(billingUsageCols[6:]...))
	if err != nil {
		return 0, err
	}
	return len(rows), tx.Commit()
}

var aiCreditCols = []string{"enterprise", "usage_date", "username", "product", "sku", "model", "org_name",
	"repo_name", "cost_center", "quantity", "unit_type", "applied_cost_per_quantity", "gross_amount",
	"discount_amount", "net_amount", "total_monthly_quota", "aic_quantity", "aic_gross_amount",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"}

// ReplaceAICredits replaces the enterprise's ai_credit rows dated from..to
// (inclusive) with rows, in one transaction. Rows sharing the natural key are
// summed. When settle is set, from is the first day of a closed month that
// rows cover in full, and the month is recorded in ai_credit_months. It
// returns the number of rows written.
func (s *Store) ReplaceAICredits(ctx context.Context, enterprise string, from, to time.Time, rows []github.AICreditRow, settle bool) (int, error) {
	type key struct {
		day                                      time.Time
		user, product, sku, model, org, repo, cc string
	}
	merged := map[key]*github.AICreditRow{}
	var order []key
	for _, r := range rows {
		day, err := time.Parse(time.DateOnly, r.Date)
		if err != nil {
			return 0, fmt.Errorf("ai_credit row date %q: %w", r.Date, err)
		}
		k := key{day, r.Username, r.Product, r.SKU, r.Model, r.Organization, r.Repository, r.CostCenterName}
		if cur, ok := merged[k]; ok {
			cur.Quantity += r.Quantity
			cur.GrossAmount += r.GrossAmount
			cur.DiscountAmount += r.DiscountAmount
			cur.NetAmount += r.NetAmount
			cur.AICQuantity += r.AICQuantity
			cur.AICGrossAmount += r.AICGrossAmount
			cur.InputTokens += r.InputTokens
			cur.OutputTokens += r.OutputTokens
			cur.CacheReadTokens += r.CacheReadTokens
			cur.CacheWriteTokens += r.CacheWriteTokens
			continue
		}
		merged[k] = &r
		order = append(order, k)
	}
	values := make([][]any, 0, len(order))
	for _, k := range order {
		r := merged[k]
		values = append(values, []any{enterprise, k.day.Format(time.DateOnly), k.user, k.product, k.sku, k.model,
			k.org, k.repo, k.cc, num(r.Quantity), nullIfEmpty(r.UnitType), num(r.AppliedCostPerQuantity),
			num(r.GrossAmount), num(r.DiscountAmount), num(r.NetAmount), num(r.TotalMonthlyQuota),
			num(r.AICQuantity), num(r.AICGrossAmount), num(r.InputTokens), num(r.OutputTokens),
			num(r.CacheReadTokens), num(r.CacheWriteTokens)})
	}

	f, t := dates(from, to)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if !settle {
		if err := keepCreditSums(ctx, tx, enterprise, f, t); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_credit_usage WHERE enterprise = $1 AND usage_date BETWEEN $2 AND $3`, enterprise, f, t); err != nil {
		return 0, fmt.Errorf("delete ai_credit rows: %w", err)
	}
	// A row dated outside from..to would collide with a kept row, so the
	// insert updates instead of failing.
	err = writeRows(ctx, tx, "ai_credit_usage", aiCreditCols, values,
		"ON CONFLICT (enterprise, usage_date, username, product, sku, model, org_name, repo_name, cost_center) DO UPDATE SET "+
			excludedSet(aiCreditCols[9:]...))
	if err != nil {
		return 0, err
	}
	if !settle {
		if err := saveCreditIncrements(ctx, tx, enterprise, f, t); err != nil {
			return 0, err
		}
	}
	if settle {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ai_credit_months (enterprise, month, rows) VALUES ($1, $2, $3)
			 ON CONFLICT (enterprise, month) DO UPDATE SET rows = EXCLUDED.rows, settled_at = now()`,
			enterprise, f, len(values)); err != nil {
			return 0, fmt.Errorf("record settled month: %w", err)
		}
	}
	return len(values), tx.Commit()
}

const creditKeySums = `SELECT username, model, org_name, sum(quantity) AS quantity,
	coalesce(sum(gross_amount), 0) AS gross_amount, coalesce(sum(discount_amount), 0) AS discount_amount,
	coalesce(sum(net_amount), 0) AS net_amount
	FROM ai_credit_usage WHERE enterprise = $1 AND usage_date BETWEEN $2 AND $3
	GROUP BY username, model, org_name`

// CREATE TABLE AS cannot bind the date range, so fill the transaction's
// temporary table with a separate INSERT.
func keepCreditSums(ctx context.Context, tx *sql.Tx, enterprise, from, to string) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE ai_credit_before (
		username TEXT, model TEXT, org_name TEXT, quantity NUMERIC, gross_amount NUMERIC,
		discount_amount NUMERIC, net_amount NUMERIC) ON COMMIT DROP`); err != nil {
		return fmt.Errorf("create ai_credit_before: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_credit_before `+creditKeySums, enterprise, from, to); err != nil {
		return fmt.Errorf("keep ai_credit sums: %w", err)
	}
	return nil
}

// saveCreditIncrements records how far the window's sums moved from
// ai_credit_before. Nothing is recorded when the window was empty before,
// since the whole month to date would then land in one hour.
func saveCreditIncrements(ctx context.Context, tx *sql.Tx, enterprise, from, to string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ai_credit_increments
		(enterprise, captured_at, username, model, org_name, quantity, gross_amount, discount_amount, net_amount)
		SELECT $1, now(), username, model, org_name,
		       coalesce(a.quantity, 0) - coalesce(b.quantity, 0),
		       coalesce(a.gross_amount, 0) - coalesce(b.gross_amount, 0),
		       coalesce(a.discount_amount, 0) - coalesce(b.discount_amount, 0),
		       coalesce(a.net_amount, 0) - coalesce(b.net_amount, 0)
		FROM (`+creditKeySums+`) a
		FULL JOIN ai_credit_before b USING (username, model, org_name)
		WHERE EXISTS (SELECT 1 FROM ai_credit_before)
		  AND (a.quantity, a.gross_amount, a.discount_amount, a.net_amount)
		      IS DISTINCT FROM (b.quantity, b.gross_amount, b.discount_amount, b.net_amount)`,
		enterprise, from, to)
	if err != nil {
		return fmt.Errorf("save ai_credit increments: %w", err)
	}
	return nil
}

func (s *Store) SettledAICreditMonths(ctx context.Context, enterprise string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT to_char(month, 'YYYY-MM') FROM ai_credit_months WHERE enterprise = $1`, enterprise)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[m] = true
	}
	return out, rows.Err()
}

// LegacyPRUKey names one premium_request/usage request: the enterprise-wide
// total (Org and Login empty), one org, or one user.
type LegacyPRUKey struct {
	Year  int
	Month int
	Org   string
	Login string
}

// LegacyPRUFetched returns the requests already stored for the enterprise.
func (s *Store) LegacyPRUFetched(ctx context.Context, enterprise string) (map[LegacyPRUKey]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT year, month, org_name, login FROM premium_request_legacy_fetches WHERE enterprise = $1`, enterprise)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[LegacyPRUKey]bool{}
	for rows.Next() {
		var k LegacyPRUKey
		if err := rows.Scan(&k.Year, &k.Month, &k.Org, &k.Login); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

var legacyPRUCols = []string{"enterprise", "year", "month", "org_name", "login", "product", "sku", "model",
	"unit_type", "price_per_unit", "gross_quantity", "gross_amount", "discount_quantity", "discount_amount",
	"net_quantity", "net_amount"}

// SaveLegacyPRU replaces one request's premium request rows and records the
// request as fetched, in one transaction. Items sharing (product, sku,
// model) are summed.
func (s *Store) SaveLegacyPRU(ctx context.Context, enterprise string, k LegacyPRUKey, items []github.PremiumRequestUsageItem) error {
	type key struct{ product, sku, model string }
	merged := map[key]*github.PremiumRequestUsageItem{}
	var order []key
	for _, it := range items {
		ik := key{it.Product, it.SKU, it.Model}
		if cur, ok := merged[ik]; ok {
			cur.GrossQuantity += it.GrossQuantity
			cur.GrossAmount += it.GrossAmount
			cur.DiscountQuantity += it.DiscountQuantity
			cur.DiscountAmount += it.DiscountAmount
			cur.NetQuantity += it.NetQuantity
			cur.NetAmount += it.NetAmount
			continue
		}
		merged[ik] = &it
		order = append(order, ik)
	}
	values := make([][]any, 0, len(order))
	for _, ik := range order {
		it := merged[ik]
		values = append(values, []any{enterprise, k.Year, k.Month, k.Org, k.Login, ik.product, ik.sku, ik.model,
			nullIfEmpty(it.UnitType), num(it.PricePerUnit), num(it.GrossQuantity), num(it.GrossAmount),
			num(it.DiscountQuantity), num(it.DiscountAmount), num(it.NetQuantity), num(it.NetAmount)})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM premium_request_usage_legacy
		 WHERE enterprise = $1 AND year = $2 AND month = $3 AND org_name = $4 AND login = $5`,
		enterprise, k.Year, k.Month, k.Org, k.Login); err != nil {
		return fmt.Errorf("delete premium request rows: %w", err)
	}
	if err := insertRows(ctx, tx, "premium_request_usage_legacy", legacyPRUCols, values); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO premium_request_legacy_fetches (enterprise, year, month, org_name, login, items)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (enterprise, year, month, org_name, login) DO UPDATE SET items = EXCLUDED.items, fetched_at = now()`,
		enterprise, k.Year, k.Month, k.Org, k.Login, len(values)); err != nil {
		return fmt.Errorf("record premium request fetch: %w", err)
	}
	return tx.Commit()
}

// UserLogins lists the logins of every known org member.
func (s *Store) UserLogins(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT login FROM github_users ORDER BY lower(login)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
