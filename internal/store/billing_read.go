package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Billing read models are JSON objects built in SQL so NUMERIC amounts keep
// their exact decimal value. Amount and quantity names are the snake_case
// column names, like the report metrics.

// MonthFilter narrows monthly billing rows. Zero values do not filter.
type MonthFilter struct {
	Year  int
	Month int
}

func (m MonthFilter) where(yearCol, monthCol string, args *[]any) string {
	var parts []string
	if m.Year > 0 {
		*args = append(*args, m.Year)
		parts = append(parts, fmt.Sprintf("%s = $%d", yearCol, len(*args)))
	}
	if m.Month > 0 {
		*args = append(*args, m.Month)
		parts = append(parts, fmt.Sprintf("%s = $%d", monthCol, len(*args)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " AND " + strings.Join(parts, " AND ")
}

func queryObjects(ctx context.Context, s *Store, q string, args ...any) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out, _, err := scanObjects(rows, false)
	return out, err
}

// BillingUsage lists the stored billing usage rows; org narrows them to one
// organization (case-insensitive).
func (s *Store) BillingUsage(ctx context.Context, enterprise, org string, m MonthFilter) ([]json.RawMessage, error) {
	args := []any{enterprise, org}
	q := `SELECT jsonb_build_object(
	        'month', to_char(month, 'YYYY-MM'), 'product', product, 'sku', sku, 'org', org_name,
	        'repo', repo_name, 'unitType', unit_type, 'quantity', quantity, 'price_per_unit', price_per_unit,
	        'gross_amount', gross_amount, 'discount_amount', discount_amount, 'net_amount', net_amount)
	      FROM billing_usage
	      WHERE enterprise = $1 AND ($2 = '' OR lower(org_name) = lower($2))` +
		m.where("extract(year FROM month)", "extract(month FROM month)", &args) + `
	      ORDER BY month, sku, org_name, repo_name`
	return queryObjects(ctx, s, q, args...)
}

// BillingMonths sums billing usage per month, SKU and unit over orgs and
// repositories.
func (s *Store) BillingMonths(ctx context.Context, enterprise, org string, m MonthFilter) ([]json.RawMessage, error) {
	args := []any{enterprise, org}
	q := `SELECT jsonb_build_object(
	        'month', to_char(month, 'YYYY-MM'), 'product', product, 'sku', sku, 'unitType', unit_type,
	        'quantity', sum(quantity), 'gross_amount', sum(gross_amount),
	        'discount_amount', sum(discount_amount), 'net_amount', sum(net_amount))
	      FROM billing_usage
	      WHERE enterprise = $1 AND ($2 = '' OR lower(org_name) = lower($2))` +
		m.where("extract(year FROM month)", "extract(month FROM month)", &args) + `
	      GROUP BY month, product, sku, unit_type
	      ORDER BY month, product, sku, unit_type`
	return queryObjects(ctx, s, q, args...)
}

// OrgBillingSummaries returns each org's latest copilot_billing_summary
// snapshot; org narrows it to one organization.
func (s *Store) OrgBillingSummaries(ctx context.Context, org string) ([]json.RawMessage, error) {
	return queryObjects(ctx, s,
		`SELECT DISTINCT ON (org_name) jsonb_build_object(
		   'org', org_name, 'snapshotDate', to_char(snapshot_date, 'YYYY-MM-DD'),
		   'seatBreakdown', jsonb_build_object(
		     'total', seat_total, 'addedThisCycle', seat_added_this_cycle,
		     'pendingInvitation', seat_pending_invitation, 'pendingCancellation', seat_pending_cancellation,
		     'activeThisCycle', seat_active_this_cycle, 'inactiveThisCycle', seat_inactive_this_cycle),
		   'planType', plan_type, 'seatManagementSetting', seat_management_setting, 'policies', policies)
		 FROM copilot_billing_summary
		 WHERE $1 = '' OR lower(org_name) = lower($1)
		 ORDER BY org_name, snapshot_date DESC`, org)
}

var creditGroups = map[string]string{
	"day":        "to_char(usage_date, 'YYYY-MM-DD')",
	"login":      "username",
	"product":    "product",
	"sku":        "sku",
	"model":      "model",
	"org":        "org_name",
	"repo":       "repo_name",
	"costCenter": "cost_center",
}

func CreditGroupNames() []string {
	return []string{"day", "login", "product", "sku", "model", "org", "repo", "costCenter"}
}

const creditSums = `jsonb_build_object(
	'quantity', coalesce(sum(quantity), 0), 'gross_amount', coalesce(sum(gross_amount), 0),
	'discount_amount', coalesce(sum(discount_amount), 0), 'net_amount', coalesce(sum(net_amount), 0),
	'aic_quantity', coalesce(sum(aic_quantity), 0), 'aic_gross_amount', coalesce(sum(aic_gross_amount), 0),
	'input_tokens', coalesce(sum(input_tokens), 0), 'output_tokens', coalesce(sum(output_tokens), 0),
	'cache_read_tokens', coalesce(sum(cache_read_tokens), 0), 'cache_write_tokens', coalesce(sum(cache_write_tokens), 0))`

type CreditFilter struct {
	Enterprise string
	// Org, Login and Model filter case-insensitively; empty does not filter.
	Org     string
	Login   string
	Model   string
	From    time.Time
	To      time.Time
	GroupBy []string
	// Top, when positive, keeps the groups of the Top logins or models with
	// the largest RankBy sum over the range. GroupBy must then hold exactly
	// one of login and model. The totals are not narrowed.
	Top int
	// RankBy is quantity (default) or gross_amount.
	RankBy string
	// Include adds these logins or models, matched case-insensitively, to the
	// Top ones. It needs Top.
	Include []string
	// Since, when set, also drops increments captured before it. Only
	// CreditHourly reads it, so the totals and the Top ranking follow it too.
	Since time.Time
	Page  Page
}

func CreditRankNames() []string { return []string{"quantity", "gross_amount"} }

// topClause requires where to contain every other filter before ranking.
func (f CreditFilter) topClause(table, where string, args *[]any) (string, error) {
	if f.Top <= 0 {
		if len(f.Include) > 0 {
			return "", fmt.Errorf("include needs top")
		}
		return "", nil
	}
	var entity string
	for _, g := range f.GroupBy {
		if g == "login" || g == "model" {
			if entity != "" {
				return "", fmt.Errorf("top needs one of login and model in the groups, got %v", f.GroupBy)
			}
			entity = creditGroups[g]
		}
	}
	if entity == "" {
		return "", fmt.Errorf("top needs login or model in the groups, got %v", f.GroupBy)
	}
	rank := f.RankBy
	if rank == "" {
		rank = "quantity"
	}
	if rank != "quantity" && rank != "gross_amount" {
		return "", fmt.Errorf("unknown credit rank %q", rank)
	}
	include := make([]string, len(f.Include))
	for i, v := range f.Include {
		include[i] = strings.ToLower(v)
	}
	*args = append(*args, f.Top, include)
	return fmt.Sprintf(` AND (%[1]s IN (SELECT %[1]s FROM %[2]s WHERE %[3]s
		GROUP BY %[1]s ORDER BY coalesce(sum(%[4]s), 0) DESC, %[1]s LIMIT $%[5]d)
		OR lower(%[1]s) = ANY($%[6]d))`,
		entity, table, where, rank, len(*args)-1, len(*args)), nil
}

// CreditUsage is ai_credit usage over a range: the totals, and the sums per
// distinct combination of the GroupBy values.
type CreditUsage struct {
	Totals json.RawMessage   `json:"totals"`
	Groups []json.RawMessage `json:"groups"`
}

// CreditUsage sums ai_credit rows. Groups are ordered by day when grouped by
// day, then by quantity descending; the int is the number of groups.
func (s *Store) CreditUsage(ctx context.Context, f CreditFilter) (*CreditUsage, int, error) {
	from, to := dates(f.From, f.To)
	args := []any{f.Enterprise, from, to, f.Org, f.Login, f.Model}
	where := `enterprise = $1 AND usage_date BETWEEN $2 AND $3
	  AND ($4 = '' OR lower(org_name) = lower($4))
	  AND ($5 = '' OR lower(username) = lower($5))
	  AND ($6 = '' OR lower(model) = lower($6))`

	out := &CreditUsage{Groups: []json.RawMessage{}}
	var totals []byte
	if err := s.db.QueryRowContext(ctx, `SELECT `+creditSums+` FROM ai_credit_usage WHERE `+where, args...).
		Scan(&totals); err != nil {
		return nil, 0, err
	}
	out.Totals = totals
	if len(f.GroupBy) == 0 {
		return out, 0, nil
	}

	keys := make([]string, 0, len(f.GroupBy))
	exprs := make([]string, 0, len(f.GroupBy))
	order := []string{}
	for _, g := range f.GroupBy {
		expr, ok := creditGroups[g]
		if !ok {
			return nil, 0, fmt.Errorf("unknown credit group %q", g)
		}
		keys = append(keys, "'"+g+"', "+expr)
		exprs = append(exprs, expr)
		if g == "day" {
			order = append(order, expr)
		}
	}
	order = append(order, "sum(quantity) DESC")
	order = append(order, exprs...)
	top, err := f.topClause("ai_credit_usage", where, &args)
	if err != nil {
		return nil, 0, err
	}
	args = append(args, f.Page.Limit, f.Page.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT jsonb_build_object(`+strings.Join(keys, ", ")+`) || jsonb_build_object('totals', `+creditSums+`),
		        count(*) OVER ()
		 FROM ai_credit_usage WHERE `+where+top+`
		 GROUP BY `+strings.Join(exprs, ", ")+`
		 ORDER BY `+strings.Join(order, ", ")+
			fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	groups, total, err := scanObjects(rows, true)
	if err != nil {
		return nil, 0, err
	}
	out.Groups = groups
	return out, total, nil
}

const incrementSums = `jsonb_build_object(
	'quantity', coalesce(sum(quantity), 0), 'gross_amount', coalesce(sum(gross_amount), 0),
	'discount_amount', coalesce(sum(discount_amount), 0), 'net_amount', coalesce(sum(net_amount), 0))`

// CreditHourlyGroupNames lists the CreditFilter.GroupBy values CreditHourly
// accepts besides the hour, which it always groups by.
func CreditHourlyGroupNames() []string { return []string{"login", "model"} }

// CreditHourly sums ai_credit_increments per UTC hour of captured_at and the
// GroupBy values. From and To are UTC days, inclusive, further narrowed by
// Since. Groups are ordered by hour, then quantity descending; the int is the
// number of groups.
func (s *Store) CreditHourly(ctx context.Context, f CreditFilter) (*CreditUsage, int, error) {
	from, to := dates(f.From, f.To)
	var since any
	if !f.Since.IsZero() {
		since = f.Since.UTC()
	}
	args := []any{f.Enterprise, from, to, f.Org, f.Login, f.Model, since}
	where := `enterprise = $1
	  AND captured_at >= $2::timestamp AT TIME ZONE 'UTC'
	  AND captured_at < ($3::date + 1)::timestamp AT TIME ZONE 'UTC'
	  AND ($4 = '' OR lower(org_name) = lower($4))
	  AND ($5 = '' OR lower(username) = lower($5))
	  AND ($6 = '' OR lower(model) = lower($6))
	  AND ($7::timestamptz IS NULL OR captured_at >= $7::timestamptz)`

	out := &CreditUsage{Groups: []json.RawMessage{}}
	var totals []byte
	if err := s.db.QueryRowContext(ctx, `SELECT `+incrementSums+` FROM ai_credit_increments WHERE `+where, args...).
		Scan(&totals); err != nil {
		return nil, 0, err
	}
	out.Totals = totals

	hour := `to_char(date_trunc('hour', captured_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:00:00"Z"')`
	keys := []string{"'hour', " + hour}
	exprs := []string{hour}
	for _, g := range f.GroupBy {
		if g != "login" && g != "model" {
			return nil, 0, fmt.Errorf("unknown hourly credit group %q", g)
		}
		keys = append(keys, "'"+g+"', "+creditGroups[g])
		exprs = append(exprs, creditGroups[g])
	}
	top, err := f.topClause("ai_credit_increments", where, &args)
	if err != nil {
		return nil, 0, err
	}
	args = append(args, f.Page.Limit, f.Page.Offset)
	rows, err := s.db.QueryContext(ctx,
		`SELECT jsonb_build_object(`+strings.Join(keys, ", ")+`) || jsonb_build_object('totals', `+incrementSums+`),
		        count(*) OVER ()
		 FROM ai_credit_increments WHERE `+where+top+`
		 GROUP BY `+strings.Join(exprs, ", ")+`
		 ORDER BY `+hour+`, sum(quantity) DESC, `+strings.Join(exprs, ", ")+
			fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	groups, total, err := scanObjects(rows, true)
	if err != nil {
		return nil, 0, err
	}
	out.Groups = groups
	return out, total, nil
}

// LegacyPRU lists stored premium request rows at one level: the enterprise
// total when org and login are empty, else the org's or the user's rows.
func (s *Store) LegacyPRU(ctx context.Context, enterprise, org, login string, m MonthFilter) ([]json.RawMessage, error) {
	args := []any{enterprise, org, login}
	q := `SELECT jsonb_build_object(
	        'month', format('%s-%s', year, lpad(month::text, 2, '0')), 'product', product, 'sku', sku,
	        'model', model, 'unitType', unit_type, 'price_per_unit', price_per_unit,
	        'gross_quantity', gross_quantity, 'gross_amount', gross_amount,
	        'discount_quantity', discount_quantity, 'discount_amount', discount_amount,
	        'net_quantity', net_quantity, 'net_amount', net_amount)
	      FROM premium_request_usage_legacy
	      WHERE enterprise = $1 AND lower(org_name) = lower($2) AND lower(login) = lower($3)` +
		m.where("year", "month", &args) + `
	      ORDER BY year, month, gross_quantity DESC, model`
	return queryObjects(ctx, s, q, args...)
}
