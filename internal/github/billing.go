package github

import (
	"context"
	"fmt"
	"net/url"
)

// BillingUsage fetches the enterprise usage report: one item per month,
// product, SKU, org and repository. Year and month narrow it; zero values
// leave GitHub's default (year to date).
func (c *Client) BillingUsage(ctx context.Context, enterprise string, year, month int) (*BillingUsageResponse, error) {
	q := url.Values{}
	if year > 0 {
		q.Set("year", itoa(year))
	}
	if month > 0 {
		q.Set("month", itoa(month))
	}
	path := withQuery(scopeBase(Enterprise(enterprise))+"/settings/billing/usage", q)
	var out BillingUsageResponse
	if err := c.getJSON(ctx, Enterprise(enterprise), familyBillingUsage, path, &out); err != nil {
		return nil, fmt.Errorf("billing usage: %w", err)
	}
	return &out, nil
}

// PremiumRequestQuery filters the retired premium request usage report.
// Organization and User are optional and only valid on the enterprise scope.
type PremiumRequestQuery struct {
	Year         int
	Month        int
	Organization string
	User         string
}

// PremiumRequestUsage fetches one month of legacy usage. GitHub serves only
// 2026-01 through 2026-05 after switching to AI credits on 2026-06-01.
func (c *Client) PremiumRequestUsage(ctx context.Context, scope Scope, pq PremiumRequestQuery) (*PremiumRequestUsage, error) {
	q := url.Values{}
	q.Set("year", itoa(pq.Year))
	q.Set("month", itoa(pq.Month))
	if pq.Organization != "" {
		q.Set("organization", pq.Organization)
	}
	if pq.User != "" {
		q.Set("user", pq.User)
	}
	base := "/organizations/" + url.PathEscape(scope.Name)
	if scope.Kind == ScopeEnterprise {
		base = scopeBase(scope)
	}
	path := withQuery(base+"/settings/billing/premium_request/usage", q)
	var out PremiumRequestUsage
	if err := c.getJSON(ctx, scope, familyPremiumRequest, path, &out); err != nil {
		return nil, fmt.Errorf("premium request usage: %w", err)
	}
	return &out, nil
}
