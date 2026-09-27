package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
)

var defaultCreditGroups = []string{"login"}

// BillingOverview is the billing view of one scope.
type BillingOverview struct {
	// Months sums the Copilot billing usage per month and SKU.
	Months []json.RawMessage `json:"months"`
	// Seats holds the latest org billing summary of the scope's org, or of
	// every org for the enterprise, one entry per org.
	Seats []json.RawMessage `json:"seats"`
}

// monthFilter parses ?year= (default: the current year when defaultYear) and
// ?month= (1-12, needs a year).
func (a *API) monthFilter(c *gin.Context, defaultYear bool) (store.MonthFilter, error) {
	var m store.MonthFilter
	if raw := c.Query("year"); raw != "" {
		y, err := strconv.Atoi(raw)
		if err != nil || y < 2000 || y > 2100 {
			return m, badRequest("invalid_year", "year must be a four-digit year")
		}
		m.Year = y
	} else if defaultYear {
		m.Year = a.now().UTC().Year()
	}
	if raw := c.Query("month"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 12 {
			return m, badRequest("invalid_month", "month must be between 1 and 12")
		}
		if m.Year == 0 {
			return m, badRequest("year_required", "month needs year")
		}
		m.Month = n
	}
	return m, nil
}

func scopeOrg(sc gh.Scope) string {
	if sc.Kind == gh.ScopeOrg {
		return sc.Name
	}
	return ""
}

func (a *API) billingMeta(c *gin.Context, sc gh.Scope, m store.MonthFilter, collectors ...string) (*Meta, error) {
	meta, err := a.meta(c.Request.Context(), collectors...)
	if err != nil {
		return nil, err
	}
	meta.Scope = sc.String()
	if m.Year > 0 {
		from := time.Date(m.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
		to := from.AddDate(1, 0, -1)
		if m.Month > 0 {
			from = time.Date(m.Year, time.Month(m.Month), 1, 0, 0, 0, 0, time.UTC)
			to = from.AddDate(0, 1, -1)
		}
		setRange(meta, from, to)
	}
	return meta, nil
}

// BillingUsage lists the Copilot items of the enterprise billing usage
// report: ?scope=&year=&month=. An org scope narrows them to that org.
func (a *API) BillingUsage(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	m, err := a.monthFilter(c, true)
	if err != nil {
		fail(c, err)
		return
	}
	rows, err := a.st.BillingUsage(ctx, a.enterprise, scopeOrg(sc), m)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.billingMeta(c, sc, m, "billing")
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, rows, meta)
}

// BillingSummary returns monthly Copilot spend per SKU and the latest org
// seat breakdowns: ?scope=&year=&month=.
func (a *API) BillingSummary(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	m, err := a.monthFilter(c, true)
	if err != nil {
		fail(c, err)
		return
	}
	months, err := a.st.BillingMonths(ctx, a.enterprise, scopeOrg(sc), m)
	if err != nil {
		fail(c, err)
		return
	}
	seats, err := a.st.OrgBillingSummaries(ctx, scopeOrg(sc))
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.billingMeta(c, sc, m, "billing", "seats")
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, BillingOverview{Months: months, Seats: seats}, meta)
}

// creditGroupBy parses ?groupBy= (comma-separated, deduplicated); "none"
// returns only the totals.
func creditGroupBy(c *gin.Context) ([]string, error) {
	raw := strings.TrimSpace(c.Query("groupBy"))
	if raw == "" {
		return defaultCreditGroups, nil
	}
	if raw == "none" {
		return nil, nil
	}
	valid := store.CreditGroupNames()
	var out []string
	for _, g := range strings.Split(raw, ",") {
		g = strings.TrimSpace(g)
		if !slices.Contains(valid, g) {
			return nil, badRequest("invalid_group", "groupBy takes none or a comma-separated list of "+strings.Join(valid, ", "))
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (a *API) creditRange(c *gin.Context) (time.Time, time.Time, error) {
	if c.Query("year") == "" && c.Query("month") == "" {
		return a.dateRange(c)
	}
	m, err := a.monthFilter(c, false)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from := time.Date(m.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(1, 0, -1)
	if m.Month > 0 {
		from = time.Date(m.Year, time.Month(m.Month), 1, 0, 0, 0, 0, time.UTC)
		to = from.AddDate(0, 1, -1)
	}
	return from, to, nil
}

// hourlyGroupBy parses ?groupBy= of /credits/hourly: none (default), login or
// model. The hour is always a group.
func hourlyGroupBy(c *gin.Context) ([]string, error) {
	raw := strings.TrimSpace(c.Query("groupBy"))
	if raw == "" || raw == "none" {
		return nil, nil
	}
	if !slices.Contains(store.CreditHourlyGroupNames(), raw) {
		return nil, badRequest("invalid_group", "groupBy takes none, login or model")
	}
	return []string{raw}, nil
}

const maxCreditTop = 50

// creditTop parses ?top= (0-maxCreditTop) and ?rankBy=. top needs exactly
// one of login and model in groups.
func creditTop(c *gin.Context, groups []string) (int, string, error) {
	rank := c.Query("rankBy")
	if rank != "" && !slices.Contains(store.CreditRankNames(), rank) {
		return 0, "", badRequest("invalid_rank", "rankBy takes "+strings.Join(store.CreditRankNames(), " or "))
	}
	raw := c.Query("top")
	if raw == "" {
		return 0, rank, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxCreditTop {
		return 0, "", badRequest("invalid_top", "top must be between 0 and "+strconv.Itoa(maxCreditTop))
	}
	entities := 0
	for _, g := range groups {
		if g == "login" || g == "model" {
			entities++
		}
	}
	if n > 0 && entities != 1 {
		return 0, "", badRequest("invalid_top", "top needs exactly one of login and model in groupBy")
	}
	return n, rank, nil
}

const maxCreditInclude = 20

// creditInclude parses ?include=, a comma-separated list of logins or models
// added to the top ones.
func creditInclude(c *gin.Context, top int) ([]string, error) {
	var out []string
	for v := range strings.SplitSeq(c.Query("include"), ",") {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) > 0 && top == 0 {
		return nil, badRequest("invalid_include", "include needs top")
	}
	if len(out) > maxCreditInclude {
		return nil, badRequest("invalid_include", "include takes at most "+strconv.Itoa(maxCreditInclude)+" names")
	}
	return out, nil
}

// credits serves the /credits routes: ?scope=&from=&to=|year=&month=
// &login=&model=&groupBy=&top=&rankBy=&include= with paging over the groups. An org
// scope narrows the rows to that org.
func (a *API) credits(c *gin.Context, groupBy func(*gin.Context) ([]string, error), alwaysGrouped bool,
	read func(context.Context, store.CreditFilter) (*store.CreditUsage, int, error)) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.creditRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	groups, err := groupBy(c)
	if err != nil {
		fail(c, err)
		return
	}
	top, rank, err := creditTop(c, groups)
	if err != nil {
		fail(c, err)
		return
	}
	include, err := creditInclude(c, top)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	usage, total, err := read(ctx, store.CreditFilter{
		Enterprise: a.enterprise,
		Org:        scopeOrg(sc),
		Login:      strings.TrimSpace(c.Query("login")),
		Model:      strings.TrimSpace(c.Query("model")),
		From:       from,
		To:         to,
		GroupBy:    groups,
		Top:        top,
		RankBy:     rank,
		Include:    include,
		Page:       p,
	})
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "aicredit")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	setRange(meta, from, to)
	if alwaysGrouped || len(groups) > 0 {
		meta.Page = pageMeta(p, total)
	}
	respond(c, usage, meta)
}

func (a *API) CreditsUsage(c *gin.Context) {
	a.credits(c, creditGroupBy, false, a.st.CreditUsage)
}

// CreditsHourly groups observed ai_credit growth by UTC hour, optionally by
// login or model. The hour records when a run saw the change.
func (a *API) CreditsHourly(c *gin.Context) {
	a.credits(c, hourlyGroupBy, true, a.st.CreditHourly)
}

// Budgets lists the enterprise's spending budgets with spend in one month:
// ?year=&month=, default the current month. The consumed amount of an
// enterprise AI credit budget is the month's net ai_credit amount, up to
// today; GitHub's own consumed figures exist for the current month only.
// Amounts are the budgets as they are now.
func (a *API) Budgets(c *gin.Context) {
	ctx := c.Request.Context()
	n := a.now().UTC()
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	thisMonth := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := thisMonth
	if c.Query("year") != "" || c.Query("month") != "" {
		m, err := a.monthFilter(c, false)
		if err != nil {
			fail(c, err)
			return
		}
		if m.Month == 0 {
			fail(c, badRequest("month_required", "year needs month"))
			return
		}
		from = time.Date(m.Year, time.Month(m.Month), 1, 0, 0, 0, 0, time.UTC)
	}
	if from.After(thisMonth) {
		fail(c, badRequest("invalid_month", "month must not be in the future"))
		return
	}
	to := from.AddDate(0, 1, -1)
	if to.After(today) {
		to = today
	}
	budgets, err := a.st.Budgets(ctx, a.enterprise, from, to, from.Equal(thisMonth))
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "budgets", "aicredit")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = gh.Enterprise(a.enterprise).String()
	setRange(meta, from, to)
	respond(c, budgets, meta)
}

// PremiumRequests lists the legacy premium request usage (2026-01..05):
// ?scope=&year=&month=&login=. The enterprise scope returns the enterprise
// total, or one user's rows with login; an org scope returns the org's rows.
func (a *API) PremiumRequests(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	m, err := a.monthFilter(c, false)
	if err != nil {
		fail(c, err)
		return
	}
	login := strings.TrimSpace(c.Query("login"))
	if login != "" && sc.Kind != gh.ScopeEnterprise {
		fail(c, badRequest("enterprise_scope_required", "per-user premium requests are stored at enterprise scope"))
		return
	}
	rows, err := a.st.LegacyPRU(ctx, a.enterprise, scopeOrg(sc), login, m)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.billingMeta(c, sc, m, "premium-legacy")
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, rows, meta)
}
