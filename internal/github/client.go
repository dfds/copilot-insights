// Package github is the GitHub API client for copilot-insights. Org and
// enterprise endpoints use the GitHub App's installation tokens. The optional
// PAT covers enterprise Copilot seats, which no App permission grants, and is
// the fallback when an App token is refused.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"go.dfds.cloud/copilot-insights/internal/metrics"
	"go.uber.org/zap"
)

const (
	defaultBaseURL         = "https://api.github.com"
	defaultDownloadTimeout = 5 * time.Minute
	userAgent              = "copilot-insights"
)

// Endpoint families key sticky PAT fallback and label metrics.
const (
	familyInstallations  = "installations"
	familyMembers        = "members"
	familySeats          = "seats"
	familyCopilotBilling = "copilot_billing"
	familyReports        = "reports"
	familyBillingUsage   = "billing_usage"
	familyPremiumRequest = "premium_request"
	familyBillingReports = "billing_reports"
	familyBudgets        = "budgets"
	familyDownload       = "download"
)

type Options struct {
	AppID         int64
	AppPrivateKey []byte
	PAT           string // optional
	APIVersion    string
	Timeout       time.Duration // per API request

	Logger  *zap.Logger
	Metrics *metrics.Metrics

	// Zero-valued hooks use production behavior.
	BaseURL               string
	Transport             http.RoundTripper
	DownloadTimeout       time.Duration
	Sleep                 Sleeper
	Now                   func() time.Time
	ReportPollInterval    time.Duration
	ReportPollMaxAttempts int
}

// Client is safe for concurrent use.
type Client struct {
	baseURL    string
	apiVersion string
	logger     *zap.Logger
	metrics    *metrics.Metrics
	sleep      Sleeper
	now        func() time.Time

	appsTransport *ghinstallation.AppsTransport
	appJWT        *credential
	pat           *credential // nil without a PAT
	timeout       time.Duration
	download      *http.Client

	pollInterval    time.Duration
	pollMaxAttempts int

	mu            sync.RWMutex
	installations map[string]*credential // key: lower-cased Scope.String()

	// patSticky holds scope|family keys whose App request was refused and
	// the PAT succeeded; later calls go straight to the PAT.
	patSticky sync.Map
}

func New(o Options) (*Client, error) {
	if o.Logger == nil || o.Metrics == nil {
		return nil, fmt.Errorf("github: Logger and Metrics are required")
	}
	c := &Client{
		baseURL:         strings.TrimRight(o.BaseURL, "/"),
		apiVersion:      o.APIVersion,
		logger:          o.Logger,
		metrics:         o.Metrics,
		sleep:           o.Sleep,
		now:             o.Now,
		timeout:         o.Timeout,
		pollInterval:    o.ReportPollInterval,
		pollMaxAttempts: o.ReportPollMaxAttempts,
		installations:   map[string]*credential{},
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	if c.sleep == nil {
		c.sleep = sleepCtx
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.timeout == 0 {
		c.timeout = 60 * time.Second
	}
	if c.pollInterval == 0 {
		c.pollInterval = reportPollInterval
	}
	if c.pollMaxAttempts == 0 {
		c.pollMaxAttempts = reportPollMaxAttempts
	}
	base := o.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	dlTimeout := o.DownloadTimeout
	if dlTimeout == 0 {
		dlTimeout = defaultDownloadTimeout
	}
	c.download = &http.Client{Transport: base, Timeout: dlTimeout}

	atr, err := ghinstallation.NewAppsTransport(base, o.AppID, o.AppPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github App transport: %w", err)
	}
	atr.BaseURL = c.baseURL
	c.appsTransport = atr
	c.appJWT = newCredential("app-jwt", "app-jwt", &http.Client{Transport: atr, Timeout: c.timeout})

	if o.PAT != "" {
		c.pat = newCredential("pat", "pat", &http.Client{
			Transport: &patTransport{token: o.PAT, base: base},
			Timeout:   c.timeout,
		})
	}
	return c, nil
}

func (c *Client) HasPAT() bool { return c.pat != nil }

// RefreshInstallations lists the App's installations and rebuilds the
// per-scope credentials. Credentials of unchanged installations are kept so
// their cached installation tokens and rate-limit state survive.
func (c *Client) RefreshInstallations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	for page := 1; ; page++ {
		path := fmt.Sprintf("/app/installations?per_page=100&page=%d", page)
		r, err := c.do(ctx, c.appJWT, familyInstallations, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusOK {
			return nil, newStatusError(http.MethodGet, path, c.appJWT.auth, r)
		}
		var batch []installationPayload
		if err := json.Unmarshal(r.body, &batch); err != nil {
			return nil, fmt.Errorf("decode installations: %w", err)
		}
		for _, p := range batch {
			switch p.TargetType {
			case "Organization":
				all = append(all, Installation{ID: p.ID, Scope: Org(p.Account.Login), Permissions: p.Permissions})
			case "Enterprise":
				all = append(all, Installation{ID: p.ID, Scope: Enterprise(p.Account.Slug), Permissions: p.Permissions})
			}
		}
		if len(batch) < 100 {
			break
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]*credential, len(all))
	for _, in := range all {
		key := scopeKey(in.Scope)
		id := "app:" + in.Scope.String()
		if old, ok := c.installations[key]; ok && old.installationID == in.ID {
			next[key] = old
			continue
		}
		cr := newCredential("app", id, &http.Client{
			Transport: ghinstallation.NewFromAppsTransport(c.appsTransport, in.ID),
			Timeout:   c.timeout,
		})
		cr.installationID = in.ID
		next[key] = cr
	}
	c.installations = next
	return all, nil
}

func scopeKey(s Scope) string { return strings.ToLower(s.String()) }

func (c *Client) installation(s Scope) *credential {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.installations[scopeKey(s)]
}

// patOnly lists endpoint families the App cannot call on a scope kind.
// Enterprise Copilot seats need manage_billing:copilot; no App permission
// grants it.
func patOnly(kind ScopeKind, family string) bool {
	return kind == ScopeEnterprise && family == familySeats
}

// call sends an API request for a scope and returns the 2xx response. It uses
// the scope's App installation; if that is refused (404, or 403 that is not a
// rate limit) and a PAT is configured, it retries once with the PAT and, if
// the PAT succeeds, uses the PAT for that scope and family from then on.
func (c *Client) call(ctx context.Context, scope Scope, family, method, path string, body []byte) (*response, error) {
	return c.callWith(ctx, scope, family, method, path, body, true)
}

// callWith is call with the PAT fallback optional. Billing report polls turn
// it off: a fresh report id 404s for a few polls, which is not a refusal.
func (c *Client) callWith(ctx context.Context, scope Scope, family, method, path string, body []byte, allowFallback bool) (*response, error) {
	primary, fallback, err := c.credentialsFor(scope, family)
	if err != nil {
		return nil, err
	}
	if !allowFallback {
		fallback = nil
	}
	r, err := c.do(ctx, primary, family, method, path, body)
	if err != nil {
		return nil, err
	}
	if fallback != nil && refused(r) {
		fr, err := c.do(ctx, fallback, family, method, path, body)
		if err != nil {
			return nil, err
		}
		if fr.status/100 == 2 {
			key := scopeKey(scope) + "|" + family
			if _, loaded := c.patSticky.LoadOrStore(key, true); !loaded {
				c.metrics.GitHubAuthFallback.WithLabelValues(family).Inc()
				c.logger.Warn("github App token refused, using the PAT for this scope and endpoint family from now on",
					zap.String("scope", scope.String()), zap.String("family", family), zap.Int("appStatus", r.status))
			}
		}
		r, primary = fr, fallback
	}
	if r.status/100 != 2 {
		return nil, newStatusError(method, path, primary.auth, r)
	}
	return r, nil
}

func (c *Client) credentialsFor(scope Scope, family string) (primary, fallback *credential, err error) {
	if patOnly(scope.Kind, family) {
		if c.pat == nil {
			return nil, nil, ErrNoPAT
		}
		return c.pat, nil, nil
	}
	app := c.installation(scope)
	if app == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoInstallation, scope)
	}
	if c.pat != nil {
		if _, ok := c.patSticky.Load(scopeKey(scope) + "|" + family); ok {
			return c.pat, nil, nil
		}
	}
	return app, c.pat, nil
}

func refused(r *response) bool {
	switch r.status {
	case http.StatusNotFound:
		return true
	case http.StatusForbidden:
		return !rateLimited(r.status, r.header, r.body)
	}
	return false
}

// getJSON calls GET path and decodes a 200 body into out.
func (c *Client) getJSON(ctx context.Context, scope Scope, family, path string, out any) error {
	r, err := c.call(ctx, scope, family, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func scopeBase(s Scope) string {
	if s.Kind == ScopeEnterprise {
		return "/enterprises/" + url.PathEscape(s.Name)
	}
	return "/orgs/" + url.PathEscape(s.Name)
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func itoa(n int) string { return strconv.Itoa(n) }

type patTransport struct {
	token string
	base  http.RoundTripper
}

func (t *patTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
