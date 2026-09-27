package handlers

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
)

const maxBreakdowns = 20

var (
	reportCollectors = []string{"reports"}
	breakdownName    = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// breakdowns parses ?breakdown=a,b (repeatable). Names are the totals_by_*
// suffixes plus pr_comment_type and extra; unknown names match nothing.
func breakdowns(c *gin.Context) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range c.QueryArray("breakdown") {
		for _, b := range strings.Split(raw, ",") {
			b = strings.TrimSpace(b)
			if b == "" || seen[b] {
				continue
			}
			if !breakdownName.MatchString(b) {
				return nil, badRequest("invalid_breakdown", "breakdown names are lower-case letters, digits and _")
			}
			seen[b] = true
			out = append(out, b)
		}
	}
	if len(out) > maxBreakdowns {
		return nil, badRequest("invalid_breakdown", "at most 20 breakdowns")
	}
	return out, nil
}

func withBreakdowns(rows []json.RawMessage, facts []store.FactRow) ([]json.RawMessage, error) {
	byDay := map[string]map[string][]store.FactRow{}
	for _, f := range facts {
		day := f.Day
		f.Day = ""
		if byDay[day] == nil {
			byDay[day] = map[string][]store.FactRow{}
		}
		byDay[day][f.Breakdown] = append(byDay[day][f.Breakdown], f)
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r, &m); err != nil {
			return nil, err
		}
		var day string
		_ = json.Unmarshal(m["day"], &day)
		b := byDay[day]
		if b == nil {
			b = map[string][]store.FactRow{}
		}
		enc, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		m["breakdowns"] = enc
		row, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (a *API) Summary(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	sum, err := a.st.Summary(ctx, sc, from, to)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "reports", "seats")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	setRange(meta, from, to)
	respond(c, sum, meta)
}

func (a *API) OrganizationDaily(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	bds, err := breakdowns(c)
	if err != nil {
		fail(c, err)
		return
	}
	rows, err := a.st.DailyReports(ctx, sc, from, to)
	if err != nil {
		fail(c, err)
		return
	}
	if len(bds) > 0 {
		facts, err := a.st.DailyFacts(ctx, sc, from, to, bds)
		if err != nil {
			fail(c, err)
			return
		}
		if rows, err = withBreakdowns(rows, facts); err != nil {
			fail(c, err)
			return
		}
	}
	a.respondReport(c, sc, rows, nil, from, to)
}

// Organization28d returns the latest 28-day feature engagement ending on or
// before ?to= (default today), or null when none is stored.
func (a *API) Organization28d(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	_, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	e, err := a.st.Engagement(ctx, sc, to)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, reportCollectors...)
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	meta.To = to.Format(time.DateOnly)
	respond(c, e, meta)
}

// UsersDaily lists users-1-day rows: ?scope=&from=&to=&login=&breakdown= with
// paging. breakdown needs login, since facts are fetched for one user.
func (a *API) UsersDaily(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	bds, err := breakdowns(c)
	if err != nil {
		fail(c, err)
		return
	}
	login := strings.TrimSpace(c.Query("login"))
	if len(bds) > 0 && login == "" {
		fail(c, badRequest("login_required", "breakdown is only served for a single login"))
		return
	}
	rows, total, err := a.st.UserDaily(ctx, store.UserDailyFilter{Scope: sc, From: from, To: to, Login: login, Page: p})
	if err != nil {
		fail(c, err)
		return
	}
	if len(bds) > 0 && len(rows) > 0 {
		var first struct {
			UserID int64 `json:"user_id"`
		}
		if err := json.Unmarshal(rows[0], &first); err != nil {
			fail(c, err)
			return
		}
		facts, err := a.st.UserFacts(ctx, sc, first.UserID, from, to, bds)
		if err != nil {
			fail(c, err)
			return
		}
		if rows, err = withBreakdowns(rows, facts); err != nil {
			fail(c, err)
			return
		}
	}
	a.respondReport(c, sc, rows, pageMeta(p, total), from, to)
}

// UsersTotals aggregates the users reports per user over the range:
// ?scope=&from=&to=&login= with paging.
func (a *API) UsersTotals(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	rows, total, err := a.st.UserTotals(ctx, sc, from, to, 0, strings.TrimSpace(c.Query("login")), p)
	if err != nil {
		fail(c, err)
		return
	}
	a.respondReport(c, sc, rows, pageMeta(p, total), from, to)
}

// ReposDaily lists repos-1-day rows: ?scope=&from=&to=&repo= with paging.
func (a *API) ReposDaily(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	rows, total, err := a.st.RepoDaily(ctx, sc, from, to, strings.TrimSpace(c.Query("repo")), p)
	if err != nil {
		fail(c, err)
		return
	}
	a.respondReport(c, sc, rows, pageMeta(p, total), from, to)
}

func (a *API) ReposTotals(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	rows, total, err := a.st.RepoTotals(ctx, sc, from, to, p)
	if err != nil {
		fail(c, err)
		return
	}
	a.respondReport(c, sc, rows, pageMeta(p, total), from, to)
}

// TeamsDaily returns per-team daily figures for an org: ?scope=org:<name>&from=&to=&team=.
func (a *API) TeamsDaily(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	if sc.Kind != gh.ScopeOrg {
		fail(c, badRequest("org_scope_required", "team reports exist only for org scopes"))
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	rows, err := a.st.TeamDaily(ctx, sc.Name, from, to, strings.TrimSpace(c.Query("team")))
	if err != nil {
		fail(c, err)
		return
	}
	a.respondReport(c, sc, rows, nil, from, to)
}

func (a *API) respondReport(c *gin.Context, sc gh.Scope, data any, p *PageMeta, from, to time.Time) {
	meta, err := a.meta(c.Request.Context(), reportCollectors...)
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	setRange(meta, from, to)
	meta.Page = p
	respond(c, data, meta)
}
