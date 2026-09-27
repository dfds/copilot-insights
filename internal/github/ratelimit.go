package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	maxRetries           = 3
	defaultFallbackSleep = 60 * time.Second
	rateLimitResetBuffer = 5 * time.Second
	serverErrorBackoff   = 2 * time.Second
	// API responses are small JSON documents; report payloads come through
	// download() with its own limit.
	maxAPIBody = 64 << 20
)

// Sleeper waits for d or context cancellation.
type Sleeper func(ctx context.Context, d time.Duration) error

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// credential is one token source. REST and GraphQL have separate rate-limit
// buckets, so remaining/reset are tracked per resource.
type credential struct {
	auth string // metric label: "app", "app-jwt" or "pat"
	id   string // rate-limit gauge label, e.g. "app:org:dfds"
	http *http.Client
	// installationID is set for App installation credentials only.
	installationID int64

	mu     sync.Mutex
	limits map[string]rateWindow
}

type rateWindow struct {
	remaining int
	reset     time.Time
}

func newCredential(auth, id string, hc *http.Client) *credential {
	return &credential{auth: auth, id: id, http: hc, limits: map[string]rateWindow{}}
}

func resourceFor(path string) string {
	if path == "/graphql" {
		return "graphql"
	}
	return "core"
}

func (cr *credential) window(resource string) (rateWindow, bool) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	w, ok := cr.limits[resource]
	return w, ok
}

func (c *Client) updateRateLimit(cr *credential, resource string, h http.Header) {
	rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	w := rateWindow{remaining: rem}
	if unix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		w.reset = time.Unix(unix, 0)
	}
	cr.mu.Lock()
	cr.limits[resource] = w
	cr.mu.Unlock()
	label := cr.id
	if resource != "core" {
		label += ":" + resource
	}
	c.metrics.GitHubRateLimitRemaining.WithLabelValues(label).Set(float64(rem))
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// rateLimited reports whether a 403/429 is a rate limit rather than a refusal.
// GitHub signals the primary limit with X-RateLimit-Remaining: 0 and the
// secondary limit with Retry-After or a "secondary rate limit" message.
func rateLimited(status int, h http.Header, body []byte) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		return h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" ||
			bytes.Contains(bytes.ToLower(body), []byte("secondary rate limit"))
	}
	return false
}

// do sends one API request with rate-limit-aware retry and returns the final
// response whatever its status; callers map non-2xx statuses to errors. A
// request that is still rate limited or failing with 5xx after maxRetries
// retries returns that last response.
func (c *Client) do(ctx context.Context, cr *credential, family, method, path string, body []byte) (*response, error) {
	resource := resourceFor(path)
	if w, ok := cr.window(resource); ok && w.remaining == 0 {
		if d := w.reset.Sub(c.now()) + rateLimitResetBuffer; d > 0 {
			c.logger.Info("waiting for github rate limit reset",
				zap.String("client", cr.id), zap.Duration("wait", d), zap.Time("resetAt", w.reset))
			if err := c.wait(ctx, "preemptive", d); err != nil {
				return nil, err
			}
		}
	}

	for attempt := 0; ; attempt++ {
		last := attempt == maxRetries
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", c.apiVersion)
		req.Header.Set("User-Agent", userAgent)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := cr.http.Do(req)
		if err != nil {
			c.metrics.GitHubRequests.WithLabelValues(family, cr.auth, "error").Inc()
			if ctx.Err() != nil || last {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			c.logger.Warn("github request failed, retrying",
				zap.String("path", path), zap.Error(err), zap.Int("attempt", attempt+1))
			if err := c.wait(ctx, "transport", backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s %s: read body: %w", method, path, err)
		}
		c.metrics.GitHubRequests.WithLabelValues(family, cr.auth, strconv.Itoa(resp.StatusCode)).Inc()
		c.updateRateLimit(cr, resource, resp.Header)
		out := &response{status: resp.StatusCode, header: resp.Header, body: b}

		var reason string
		var d time.Duration
		switch {
		case !rateLimited(resp.StatusCode, resp.Header, b):
			if resp.StatusCode < 500 {
				return out, nil
			}
			reason, d = "server_error", backoff(attempt)
		case resp.Header.Get("X-RateLimit-Remaining") == "0" && resp.Header.Get("Retry-After") == "":
			reason, d = "primary", c.untilReset(resp.Header)
		default:
			reason, d = "secondary", retryAfter(resp.Header)
		}
		if last {
			return out, nil
		}
		c.logger.Warn("github request throttled, retrying",
			zap.String("path", path), zap.Int("status", resp.StatusCode), zap.String("reason", reason),
			zap.Duration("wait", d), zap.Int("retriesRemaining", maxRetries-attempt))
		if err := c.wait(ctx, reason, d); err != nil {
			return nil, err
		}
	}
}

func (c *Client) wait(ctx context.Context, reason string, d time.Duration) error {
	c.metrics.GitHubRateLimitWaits.WithLabelValues(reason).Inc()
	return c.sleep(ctx, d)
}

func (c *Client) untilReset(h http.Header) time.Duration {
	if unix, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if d := time.Unix(unix, 0).Sub(c.now()) + rateLimitResetBuffer; d > 0 {
			return d
		}
	}
	return defaultFallbackSleep
}

func retryAfter(h http.Header) time.Duration {
	if secs, err := strconv.ParseInt(h.Get("Retry-After"), 10, 64); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return defaultFallbackSleep
}

func backoff(attempt int) time.Duration {
	return serverErrorBackoff << attempt
}

// StatusError is a non-2xx GitHub API response.
type StatusError struct {
	Method  string
	Path    string
	Status  int
	Auth    string
	Message string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("github %s %s: status %d (%s auth)", e.Method, e.Path, e.Status, e.Auth)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// Is makes errors.Is(err, ErrNotFound) match a 404.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

var (
	// ErrNotFound is a 404. On Copilot endpoints it means Copilot is not
	// enabled for the org or the endpoint is not available to the credential.
	ErrNotFound = errors.New("github: not found")
	// ErrNoPAT is returned by PAT-only endpoints when no PAT is configured.
	ErrNoPAT = errors.New("github: endpoint needs CPI_GITHUB_PAT, which is not set")
	// ErrNoInstallation means the App is not installed on the scope.
	ErrNoInstallation = errors.New("github: App is not installed on scope")
)

func newStatusError(method, path, auth string, r *response) *StatusError {
	e := &StatusError{Method: method, Path: path, Status: r.status, Auth: auth}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(r.body, &m) == nil {
		e.Message = truncate(m.Message, 200)
	} else {
		e.Message = truncate(strings.TrimSpace(string(r.body)), 200)
	}
	return e
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
