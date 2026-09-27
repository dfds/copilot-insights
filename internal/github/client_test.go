package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRefreshInstallationsFromFixture(t *testing.T) {
	env := newEnv(t, "")
	got, err := env.client.RefreshInstallations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	for _, in := range got {
		scopes = append(scopes, fmt.Sprintf("%s=%d", in.Scope, in.ID))
	}
	want := []string{"enterprise:team-1=693811", "org:org-2=480569", "org:org-1=594371"}
	if !slices.Equal(scopes, want) {
		t.Fatalf("installations = %v, want %v", scopes, want)
	}
	if got[0].Permissions["enterprise_copilot_metrics"] != "read" {
		t.Errorf("enterprise permissions not decoded: %v", got[0].Permissions)
	}
	if a := env.fake.auths("/app/installations"); a[0] != "jwt" {
		t.Errorf("installations listed with %q auth, want the App JWT", a[0])
	}
}

func TestRefreshInstallationsPages(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		n := 100
		if page == 2 {
			n = 1
		}
		var items []string
		for i := range n {
			items = append(items, fmt.Sprintf(`{"id":%d,"account":{"login":"org-%d-%d"},"target_type":"Organization"}`, page*1000+i, page, i))
		}
		w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	})
	got, err := env.client.RefreshInstallations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 101 {
		t.Fatalf("got %d installations, want 101", len(got))
	}
	if env.client.installation(Org("org-2-0")) == nil {
		t.Error("second-page org has no credential")
	}
	if env.client.installation(Org("org-1")) != nil {
		t.Error("credential of a removed installation was kept")
	}
}

func TestOrgRequestsUseInstallationToken(t *testing.T) {
	env := newEnv(t, testPAT)
	env.fake.handle("GET /orgs/ORG-1/copilot/billing", serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")))
	if _, err := env.client.CopilotBilling(context.Background(), "ORG-1"); err != nil {
		t.Fatal(err)
	}
	if a := env.fake.auths("/orgs/ORG-1/copilot/billing"); !slices.Equal(a, []string{"app:594371"}) {
		t.Fatalf("auths = %v, want the org-1 installation", a)
	}
}

func TestUninstalledOrgFails(t *testing.T) {
	env := newEnv(t, testPAT)
	_, err := env.client.CopilotBilling(context.Background(), "org-4")
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

func TestPATFallbackIsSticky(t *testing.T) {
	env := newEnv(t, testPAT)
	path := "/orgs/org-1/copilot/billing"
	env.fake.handle("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		if authKind(r.Header.Get("Authorization")) != "pat" {
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		w.Write(fixture(t, "org_copilot_billing.json"))
	})
	for range 3 {
		if _, err := env.client.CopilotBilling(context.Background(), "org-1"); err != nil {
			t.Fatal(err)
		}
	}
	if a := env.fake.auths(path); !slices.Equal(a, []string{"app:594371", "pat", "pat", "pat"}) {
		t.Fatalf("auths = %v, want one App attempt then PAT only", a)
	}
	if n := testutil.ToFloat64(env.metrics.GitHubAuthFallback.WithLabelValues(familyCopilotBilling)); n != 1 {
		t.Errorf("fallback metric = %v, want 1", n)
	}
	// Stickiness is per scope: org-2 still starts with the App.
	env.fake.handle("GET /orgs/org-2/copilot/billing", serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")))
	if _, err := env.client.CopilotBilling(context.Background(), "org-2"); err != nil {
		t.Fatal(err)
	}
	if a := env.fake.auths("/orgs/org-2/copilot/billing"); !slices.Equal(a, []string{"app:480569"}) {
		t.Errorf("org-2 auths = %v", a)
	}
}

func TestNotFoundOnBothCredentialsIsNotSticky(t *testing.T) {
	env := newEnv(t, testPAT)
	path := "/orgs/org-1/copilot/billing"
	env.fake.handle("GET "+path, serveBytes(http.StatusNotFound, []byte(`{"message":"Not Found"}`)))
	for range 2 {
		_, err := env.client.CopilotBilling(context.Background(), "org-1")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	}
	if a := env.fake.auths(path); !slices.Equal(a, []string{"app:594371", "pat", "app:594371", "pat"}) {
		t.Fatalf("auths = %v", a)
	}
}

func TestRefusalWithoutPATIsAnError(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/billing", serveBytes(http.StatusForbidden, []byte(`{"message":"Resource not accessible by integration"}`)))
	_, err := env.client.CopilotBilling(context.Background(), "org-1")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden || se.Auth != "app" {
		t.Fatalf("err = %v, want a 403 StatusError from the App", err)
	}
	if !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("error lacks GitHub's message: %v", err)
	}
}

func TestSecondaryRateLimitRetriesAfterRetryAfter(t *testing.T) {
	env := newEnv(t, testPAT)
	path := "/orgs/org-1/copilot/billing"
	env.fake.handle("GET "+path, sequence(
		withHeaders(serveBytes(http.StatusTooManyRequests, nil), "Retry-After", "7"),
		withHeaders(serveBytes(http.StatusForbidden, []byte(`{"message":"You have exceeded a secondary rate limit"}`))),
		serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")),
	))
	if _, err := env.client.CopilotBilling(context.Background(), "org-1"); err != nil {
		t.Fatal(err)
	}
	if s := env.sleeps.all(); !slices.Equal(s, []time.Duration{7 * time.Second, defaultFallbackSleep}) {
		t.Errorf("sleeps = %v", s)
	}
	// A rate-limited 403 is not a refusal, so the PAT is never tried.
	if a := env.fake.auths(path); !slices.Equal(a, []string{"app:594371", "app:594371", "app:594371"}) {
		t.Errorf("auths = %v", a)
	}
	if n := testutil.ToFloat64(env.metrics.GitHubRateLimitWaits.WithLabelValues("secondary")); n != 2 {
		t.Errorf("secondary waits = %v, want 2", n)
	}
}

func TestPrimaryRateLimitSleepsUntilReset(t *testing.T) {
	env := newEnv(t, "")
	reset := strconv.FormatInt(testNow.Add(30*time.Second).Unix(), 10)
	env.fake.handle("GET /orgs/org-1/copilot/billing", sequence(
		withHeaders(serveBytes(http.StatusForbidden, []byte(`{"message":"API rate limit exceeded"}`)),
			"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset),
		serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")),
	))
	if _, err := env.client.CopilotBilling(context.Background(), "org-1"); err != nil {
		t.Fatal(err)
	}
	if s := env.sleeps.all(); !slices.Equal(s, []time.Duration{30*time.Second + rateLimitResetBuffer}) {
		t.Errorf("sleeps = %v", s)
	}
}

func TestExhaustedRemainingWaitsBeforeNextRequest(t *testing.T) {
	env := newEnv(t, "")
	reset := strconv.FormatInt(testNow.Add(10*time.Second).Unix(), 10)
	env.fake.handle("GET /orgs/org-1/copilot/billing", withHeaders(serveBytes(http.StatusOK, fixture(t, "org_copilot_billing.json")),
		"X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset))
	for range 2 {
		if _, err := env.client.CopilotBilling(context.Background(), "org-1"); err != nil {
			t.Fatal(err)
		}
	}
	if s := env.sleeps.all(); !slices.Equal(s, []time.Duration{10*time.Second + rateLimitResetBuffer}) {
		t.Errorf("sleeps = %v, want one pre-emptive wait before the second call", s)
	}
	if g := testutil.ToFloat64(env.metrics.GitHubRateLimitRemaining.WithLabelValues("app:org:org-1")); g != 0 {
		t.Errorf("remaining gauge = %v", g)
	}
}

func TestServerErrorsBackOffThenFail(t *testing.T) {
	env := newEnv(t, testPAT)
	path := "/orgs/org-1/copilot/billing"
	env.fake.handle("GET "+path, serveBytes(http.StatusBadGateway, []byte("bad gateway")))
	_, err := env.client.CopilotBilling(context.Background(), "org-1")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502 StatusError", err)
	}
	if s := env.sleeps.all(); !slices.Equal(s, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}) {
		t.Errorf("sleeps = %v", s)
	}
	if n := len(env.fake.requests(path)); n != maxRetries+1 {
		t.Errorf("requests = %d, want %d", n, maxRetries+1)
	}
}

func TestRateLimitSleepStopsOnCancel(t *testing.T) {
	env := newEnv(t, "")
	env.client.sleep = sleepCtx
	env.fake.handle("GET /orgs/org-1/copilot/billing", withHeaders(serveBytes(http.StatusTooManyRequests, nil), "Retry-After", "3600"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := env.client.CopilotBilling(ctx, "org-1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("sleep did not stop on context cancellation")
	}
}

func TestRequestHeaders(t *testing.T) {
	env := newEnv(t, "")
	env.fake.handle("GET /orgs/org-1/copilot/billing", func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-GitHub-Api-Version"); v != "2026-03-10" {
			t.Errorf("api version header = %q", v)
		}
		if v := r.Header.Get("Accept"); v != "application/vnd.github+json" {
			t.Errorf("accept header = %q", v)
		}
		w.Write(fixture(t, "org_copilot_billing.json"))
	})
	if _, err := env.client.CopilotBilling(context.Background(), "org-1"); err != nil {
		t.Fatal(err)
	}
}
