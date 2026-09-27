package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
	"go.dfds.cloud/copilot-insights/internal/store"
)

// orgFallbackScope is scopes.seats when a bundle's seats are org-scope rows,
// which may come from several orgs.
const orgFallbackScope = "org:*"

const latestWindowDays = 28

var bundleBreakdowns = []string{"ide", "feature", "language_model", "model_feature", "cli"}

type UserBundle struct {
	Identity store.Identity `json:"identity"`
	Scopes   BundleScopes   `json:"scopes"`
	Seats    []store.Seat   `json:"seats"`
	// Daily holds the user's users-1-day rows in the requested range.
	Daily []json.RawMessage `json:"daily"`
	// Latest28d aggregates the 28 days ending at the range end; nil when the
	// user has no report rows then.
	Latest28d *UserPeriod `json:"latest28d"`
	// Credits sums the user's ai_credit billing rows in the range.
	Credits UserCredits `json:"credits"`
	// PremiumRequests holds the user's premium request usage for 2026-01..05,
	// before AI credits; it ignores the range.
	PremiumRequests []json.RawMessage `json:"premiumRequests"`
	// Budget is the AI credit budget covering the user in the current month,
	// whatever the range: {amount, consumed, scope, preventFurtherUsage} in
	// USD. consumed is GitHub's figure, the gross amount of the month to date.
	// Null when no budget covers the user.
	Budget json.RawMessage `json:"budget"`
}

// BundleScopes names the scope each part of the bundle was read from; ""
// when there was nothing to read. Credits and premium requests exist at
// enterprise scope only.
type BundleScopes struct {
	Seats   string `json:"seats"`
	Reports string `json:"reports"`
	Credits string `json:"credits"`
}

type UserCredits struct {
	Totals  json.RawMessage   `json:"totals"`
	ByDay   []json.RawMessage `json:"byDay"`
	ByModel []json.RawMessage `json:"byModel"`
}

type UserPeriod struct {
	From       string                     `json:"from"`
	To         string                     `json:"to"`
	Totals     store.UserTotals           `json:"totals"`
	Breakdowns map[string][]store.FactRow `json:"breakdowns"`
}

var userCollectors = []string{"members", "seats", "reports", "aicredit", "premium-legacy", "budgets"}

// ListIdentities lists the login-to-email mapping: ?email=&login=&q= with
// limit/offset paging.
func (a *API) ListIdentities(c *gin.Context) {
	ctx := c.Request.Context()
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	ids, total, err := a.st.Identities(ctx, store.IdentityFilter{
		Login: c.Query("login"),
		Email: identity.NormalizeEmail(c.Query("email")),
		Q:     strings.TrimSpace(c.Query("q")),
		Page:  p,
	})
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "members")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Page = pageMeta(p, total)
	respond(c, ids, meta)
}

func (a *API) GetUser(c *gin.Context) {
	ctx := c.Request.Context()
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	ident, err := a.st.Identity(ctx, 0, c.Param("login"))
	if err != nil {
		fail(c, err)
		return
	}
	if ident == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
		return
	}
	meta, err := a.meta(ctx, userCollectors...)
	if err != nil {
		fail(c, err)
		return
	}
	setRange(meta, from, to)
	b, err := a.bundle(ctx, *ident, meta, from, to)
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, b, meta)
}

// GetUserByEmail resolves one or more addresses (the path value, then each
// ?email= in order) to a single login and returns its bundle. Addresses
// outside the DFDS domains are ignored. The first address that maps wins;
// meta.matchedEmail names it and meta.otherLogins lists the other accounts
// that address maps to. No match is 404 not_mapped.
func (a *API) GetUserByEmail(c *gin.Context) {
	ctx := c.Request.Context()
	var raw []string
	if p := c.Param("email"); p != "" {
		raw = append(raw, p)
	}
	raw = append(raw, c.QueryArray("email")...)
	if len(raw) == 0 {
		fail(c, badRequest("email_required", "pass the address as the path value or as ?email="))
		return
	}
	var emails []string
	seen := map[string]bool{}
	for _, r := range raw {
		n := identity.NormalizeEmail(r)
		if n == "" || seen[n] || !identity.IsDfdsTenantEmail(n) {
			continue
		}
		seen[n] = true
		emails = append(emails, n)
	}
	if len(emails) == 0 {
		fail(c, badRequest("no_dfds_email", "no address is on a DFDS domain"))
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}

	for _, email := range emails {
		cands, err := a.st.EmailCandidates(ctx, email)
		if err != nil {
			fail(c, err)
			return
		}
		chosen, others, ok := identity.Pick(cands)
		if !ok {
			continue
		}
		ident, err := a.st.Identity(ctx, chosen.UserID, "")
		if err != nil {
			fail(c, err)
			return
		}
		if ident == nil {
			continue
		}
		meta, err := a.meta(ctx, userCollectors...)
		if err != nil {
			fail(c, err)
			return
		}
		setRange(meta, from, to)
		meta.MatchedEmail = email
		meta.OtherLogins = others
		b, err := a.bundle(ctx, *ident, meta, from, to)
		if err != nil {
			fail(c, err)
			return
		}
		respond(c, b, meta)
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "not_mapped"})
}

// bundle assembles a user's data for from..to. Per-user data is read at
// enterprise scope. Each part falls back to org-scope rows only when its
// enterprise collector has produced nothing: seats without a PAT, reports
// without the App's enterprise installation. Report rows are never summed
// across orgs; the fallback reads the one org holding most of the user's
// rows. data.scopes names what was read, and meta.scope is set only when both
// parts come from the same scope.
func (a *API) bundle(ctx context.Context, ident store.Identity, meta *Meta, from, to time.Time) (UserBundle, error) {
	b := UserBundle{Identity: ident, Seats: []store.Seat{}, Daily: []json.RawMessage{}}
	ent := gh.Enterprise(a.enterprise)

	seats, err := a.st.UserSeats(ctx, ident.UserID)
	if err != nil {
		return b, err
	}
	hasEnt, err := a.st.HasEnterpriseSeats(ctx, a.enterprise)
	if err != nil {
		return b, err
	}
	want := func(s store.Seat) bool { return strings.HasPrefix(s.Scope, string(gh.ScopeOrg)+":") }
	b.Scopes.Seats = orgFallbackScope
	if hasEnt {
		want = func(s store.Seat) bool { return s.Scope == ent.String() }
		b.Scopes.Seats = ent.String()
	}
	for _, s := range seats {
		if want(s) {
			b.Seats = append(b.Seats, s)
		}
	}

	winFrom := to.AddDate(0, 0, 1-latestWindowDays)
	var rs gh.Scope
	hasEntReports, err := a.st.HasUserReports(ctx, ent)
	if err != nil {
		return b, err
	}
	if hasEntReports {
		rs = ent
	} else {
		org, err := a.st.UserReportOrg(ctx, ident.UserID, earlier(from, winFrom), to)
		if err != nil {
			return b, err
		}
		if org != "" {
			rs = gh.Org(org)
		}
	}
	if rs.Name != "" {
		b.Scopes.Reports = rs.String()
		daily, _, err := a.st.UserDaily(ctx, store.UserDailyFilter{
			Scope: rs, From: from, To: to, UserID: ident.UserID, Page: store.Page{Limit: maxRangeDays},
		})
		if err != nil {
			return b, err
		}
		b.Daily = daily
		if b.Latest28d, err = a.latest28d(ctx, rs, ident.UserID, winFrom, to); err != nil {
			return b, err
		}
	}
	b.Scopes.Credits = ent.String()
	if b.Credits, err = a.userCredits(ctx, ident.Login, from, to); err != nil {
		return b, err
	}
	if b.PremiumRequests, err = a.st.LegacyPRU(ctx, a.enterprise, "", ident.Login, store.MonthFilter{}); err != nil {
		return b, err
	}
	if b.Budget, err = a.st.UserBudget(ctx, a.enterprise, ident.Login); err != nil {
		return b, err
	}
	if b.Scopes.Seats == b.Scopes.Reports && b.Scopes.Reports == b.Scopes.Credits {
		meta.Scope = b.Scopes.Seats
	}
	return b, nil
}

// The ai_credit report identifies users by login, not user ID.
func (a *API) userCredits(ctx context.Context, login string, from, to time.Time) (UserCredits, error) {
	f := store.CreditFilter{Enterprise: a.enterprise, Login: login, From: from, To: to,
		GroupBy: []string{"day"}, Page: store.Page{Limit: maxRangeDays}}
	byDay, _, err := a.st.CreditUsage(ctx, f)
	if err != nil {
		return UserCredits{}, err
	}
	f.GroupBy, f.Page = []string{"model"}, store.Page{Limit: maxLimit}
	byModel, _, err := a.st.CreditUsage(ctx, f)
	if err != nil {
		return UserCredits{}, err
	}
	return UserCredits{Totals: byDay.Totals, ByDay: byDay.Groups, ByModel: byModel.Groups}, nil
}

func (a *API) latest28d(ctx context.Context, sc gh.Scope, userID int64, from, to time.Time) (*UserPeriod, error) {
	totals, _, err := a.st.UserTotals(ctx, sc, from, to, userID, "", store.Page{Limit: 1})
	if err != nil || len(totals) == 0 {
		return nil, err
	}
	facts, err := a.st.UserFactTotals(ctx, sc, userID, from, to, bundleBreakdowns)
	if err != nil {
		return nil, err
	}
	p := &UserPeriod{
		From:       from.Format(time.DateOnly),
		To:         to.Format(time.DateOnly),
		Totals:     totals[0],
		Breakdowns: map[string][]store.FactRow{},
	}
	for _, b := range bundleBreakdowns {
		p.Breakdowns[b] = []store.FactRow{}
	}
	for _, f := range facts {
		p.Breakdowns[f.Breakdown] = append(p.Breakdowns[f.Breakdown], f)
	}
	return p, nil
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
