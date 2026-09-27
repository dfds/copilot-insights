package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
	"go.dfds.cloud/copilot-insights/internal/store"
)

const (
	defaultRangeDays = 28
	maxRangeDays     = 400
	defaultLimit     = 500
	maxLimit         = 5000
)

type Store interface {
	Scopes(ctx context.Context) ([]store.ScopeInfo, error)
	CollectorStatuses(ctx context.Context) ([]store.CollectorStatus, error)
	LastSuccess(ctx context.Context) (map[string]time.Time, error)
	Seats(ctx context.Context, f store.SeatFilter) ([]store.Seat, int, error)
	SeatHistory(ctx context.Context, scope gh.Scope, from, to time.Time) ([]store.SeatDay, error)
	UserSeats(ctx context.Context, userID int64) ([]store.Seat, error)
	HasEnterpriseSeats(ctx context.Context, enterprise string) (bool, error)
	Identities(ctx context.Context, f store.IdentityFilter) ([]store.Identity, int, error)
	Identity(ctx context.Context, userID int64, login string) (*store.Identity, error)
	EmailCandidates(ctx context.Context, normalized string) ([]identity.Candidate, error)

	Summary(ctx context.Context, scope gh.Scope, from, to time.Time) (*store.Summary, error)
	DailyReports(ctx context.Context, scope gh.Scope, from, to time.Time) ([]json.RawMessage, error)
	DailyFacts(ctx context.Context, scope gh.Scope, from, to time.Time, breakdowns []string) ([]store.FactRow, error)
	Engagement(ctx context.Context, scope gh.Scope, to time.Time) (*store.Engagement, error)
	UserDaily(ctx context.Context, f store.UserDailyFilter) ([]json.RawMessage, int, error)
	UserFacts(ctx context.Context, scope gh.Scope, userID int64, from, to time.Time, breakdowns []string) ([]store.FactRow, error)
	UserFactTotals(ctx context.Context, scope gh.Scope, userID int64, from, to time.Time, breakdowns []string) ([]store.FactRow, error)
	UserTotals(ctx context.Context, scope gh.Scope, from, to time.Time, userID int64, login string, p store.Page) ([]store.UserTotals, int, error)
	RepoDaily(ctx context.Context, scope gh.Scope, from, to time.Time, repo string, p store.Page) ([]json.RawMessage, int, error)
	RepoTotals(ctx context.Context, scope gh.Scope, from, to time.Time, p store.Page) ([]store.RepoTotals, int, error)
	TeamDaily(ctx context.Context, org string, from, to time.Time, team string) ([]store.TeamDay, error)
	HasUserReports(ctx context.Context, scope gh.Scope) (bool, error)
	UserReportOrg(ctx context.Context, userID int64, from, to time.Time) (string, error)

	BillingUsage(ctx context.Context, enterprise, org string, m store.MonthFilter) ([]json.RawMessage, error)
	BillingMonths(ctx context.Context, enterprise, org string, m store.MonthFilter) ([]json.RawMessage, error)
	OrgBillingSummaries(ctx context.Context, org string) ([]json.RawMessage, error)
	CreditUsage(ctx context.Context, f store.CreditFilter) (*store.CreditUsage, int, error)
	CreditHourly(ctx context.Context, f store.CreditFilter) (*store.CreditUsage, int, error)
	LegacyPRU(ctx context.Context, enterprise, org, login string, m store.MonthFilter) ([]json.RawMessage, error)
	UserBudget(ctx context.Context, enterprise, login string) (json.RawMessage, error)
	Budgets(ctx context.Context, enterprise string, from, to time.Time, current bool) ([]json.RawMessage, error)
}

type Collectors interface {
	Names() []string
	Trigger(name string) error
}

type API struct {
	st         Store
	collectors Collectors
	enterprise string
	now        func() time.Time
}

func NewAPI(st Store, collectors Collectors, enterprise string, now func() time.Time) *API {
	if now == nil {
		now = time.Now
	}
	return &API{st: st, collectors: collectors, enterprise: enterprise, now: now}
}

// Meta is the "meta" half of every {data, meta} response.
type Meta struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Scope       string    `json:"scope,omitempty"`
	From        string    `json:"from,omitempty"`
	To          string    `json:"to,omitempty"`
	// Freshness maps each collector feeding the route to its last ok or
	// partial finish; null when it has never succeeded.
	Freshness    map[string]*time.Time `json:"freshness"`
	Page         *PageMeta             `json:"page,omitempty"`
	MatchedEmail string                `json:"matchedEmail,omitempty"`
	OtherLogins  []string              `json:"otherLogins,omitempty"`
}

type PageMeta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// errBadRequest marks a parameter error; its text is returned to the client.
type errBadRequest struct {
	code   string
	detail string
}

func (e errBadRequest) Error() string { return e.code + ": " + e.detail }

func badRequest(code, detail string) error { return errBadRequest{code: code, detail: detail} }

var errUnknownScope = errors.New("unknown scope")

// fail writes the error response for err: 400 for parameter errors, 404 for
// an unknown scope, 500 otherwise (recorded on the context for the error
// logger, not returned to the client).
func fail(c *gin.Context, err error) {
	var br errBadRequest
	switch {
	case errors.As(err, &br):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": br.code, "detail": br.detail})
	case errors.Is(err, errUnknownScope):
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "unknown_scope"})
	default:
		_ = c.Error(err)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}

func (a *API) meta(ctx context.Context, collectors ...string) (*Meta, error) {
	last, err := a.st.LastSuccess(ctx)
	if err != nil {
		return nil, err
	}
	fresh := make(map[string]*time.Time, len(collectors))
	for _, name := range collectors {
		if t, ok := last[name]; ok {
			fresh[name] = &t
		} else {
			fresh[name] = nil
		}
	}
	return &Meta{GeneratedAt: a.now().UTC(), Freshness: fresh}, nil
}

func respond(c *gin.Context, data any, meta *Meta) {
	c.JSON(http.StatusOK, gin.H{"data": data, "meta": meta})
}

// scope parses the required scope parameter (org:<name> or
// enterprise:<name>) and checks it names a known scope.
func (a *API) scope(c *gin.Context) (gh.Scope, error) {
	raw := c.Query("scope")
	if raw == "" {
		return gh.Scope{}, badRequest("scope_required", "scope=org:<name> or scope=enterprise:<name> is required")
	}
	kind, name, ok := strings.Cut(raw, ":")
	if !ok || name == "" || (kind != string(gh.ScopeOrg) && kind != string(gh.ScopeEnterprise)) {
		return gh.Scope{}, badRequest("invalid_scope", "scope must be org:<name> or enterprise:<name>")
	}
	scopes, err := a.st.Scopes(c.Request.Context())
	if err != nil {
		return gh.Scope{}, err
	}
	for _, sc := range scopes {
		if sc.Kind == kind && strings.EqualFold(sc.Name, name) {
			return sc.Scope(), nil
		}
	}
	return gh.Scope{}, errUnknownScope
}

// dateRange parses from/to (YYYY-MM-DD, inclusive, UTC). The default is the
// 28 days ending today; the span may not exceed 400 days.
func (a *API) dateRange(c *gin.Context) (from, to time.Time, err error) {
	parse := func(name string) (time.Time, bool, error) {
		raw := c.Query(name)
		if raw == "" {
			return time.Time{}, false, nil
		}
		t, err := time.Parse(time.DateOnly, raw)
		if err != nil {
			return time.Time{}, false, badRequest("invalid_date", name+" must be YYYY-MM-DD")
		}
		return t, true, nil
	}
	to, hasTo, err := parse("to")
	if err != nil {
		return
	}
	from, hasFrom, err := parse("from")
	if err != nil {
		return
	}
	if !hasTo {
		n := a.now().UTC()
		to = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	}
	if !hasFrom {
		from = to.AddDate(0, 0, 1-defaultRangeDays)
	}
	if from.After(to) {
		return from, to, badRequest("invalid_range", "from must not be after to")
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > maxRangeDays {
		return from, to, badRequest("invalid_range", "the range may span at most "+strconv.Itoa(maxRangeDays)+" days")
	}
	return from, to, nil
}

func setRange(m *Meta, from, to time.Time) {
	m.From, m.To = from.Format(time.DateOnly), to.Format(time.DateOnly)
}

// page parses limit (default 500, max 5000) and offset.
func page(c *gin.Context) (store.Page, error) {
	p := store.Page{Limit: defaultLimit}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			return p, badRequest("invalid_limit", "limit must be between 1 and "+strconv.Itoa(maxLimit))
		}
		p.Limit = n
	}
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return p, badRequest("invalid_offset", "offset must be a non-negative integer")
		}
		p.Offset = n
	}
	return p, nil
}

func pageMeta(p store.Page, total int) *PageMeta {
	return &PageMeta{Limit: p.Limit, Offset: p.Offset, Total: total}
}
