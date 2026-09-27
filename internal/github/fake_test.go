package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.dfds.cloud/copilot-insights/internal/metrics"
	"go.uber.org/zap"
)

// testEnterprise is the enterprise slug in testdata/app_installations.json;
// the fixture redaction renamed it.
const testEnterprise = "team-1"

const testPAT = "test-pat"

var testNow = time.Date(2032, 5, 2, 12, 0, 0, 0, time.UTC)

var (
	keyOnce sync.Once
	keyPEM  []byte
)

func testKey(t *testing.T) []byte {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return keyPEM
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type recorded struct {
	Method string
	Path   string
	Query  string
	Auth   string // "app:<installation id>", "pat", "jwt" or "none"
	Body   []byte
}

// fakeGitHub routes "METHOD /path" to handlers and records every request
// except installation token exchanges, which it answers itself.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	reqs   []recorded
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.handle("GET /app/installations", serveBytes(http.StatusOK, fixture(t, "app_installations.json")))
	return f
}

func (f *fakeGitHub) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		id := strings.Split(r.URL.Path, "/")[3]
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"inst-%s","expires_at":%q}`, id, time.Now().Add(time.Hour).Format(time.RFC3339))
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: authKind(r.Header.Get("Authorization")), Body: body})
	h := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	h(w, r)
}

func authKind(h string) string {
	switch {
	case h == "":
		return "none"
	case strings.HasPrefix(h, "token inst-"):
		return "app:" + strings.TrimPrefix(h, "token inst-")
	case h == "Bearer "+testPAT:
		return "pat"
	case strings.HasPrefix(h, "Bearer "):
		return "jwt"
	}
	return "unknown"
}

func (f *fakeGitHub) requests(path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) auths(path string) []string {
	var out []string
	for _, r := range f.requests(path) {
		out = append(out, r.Auth)
	}
	return out
}

func (f *fakeGitHub) blobURL(name string) string {
	return f.srv.URL + "/blob/" + name + "?sig=SECRET"
}

func serveBytes(status int, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write(body)
	}
}

func serveJSON(status int, v any) http.HandlerFunc {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return serveBytes(status, b)
}

// sequence serves the handlers in order and repeats the last one.
func sequence(hs ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := hs[min(n, len(hs)-1)]
		n++
		mu.Unlock()
		h(w, r)
	}
}

func withHeaders(h http.HandlerFunc, kv ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < len(kv); i += 2 {
			w.Header().Set(kv[i], kv[i+1])
		}
		h(w, r)
	}
}

type sleepRecorder struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sleeps = append(s.sleeps, d)
	return nil
}

func (s *sleepRecorder) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.sleeps...)
}

type testEnv struct {
	fake    *fakeGitHub
	client  *Client
	metrics *metrics.Metrics
	sleeps  *sleepRecorder
}

func newEnv(t *testing.T, pat string) *testEnv {
	t.Helper()
	f := newFake(t)
	m := metrics.NewMetricsWith(prometheus.NewRegistry())
	s := &sleepRecorder{}
	c, err := New(Options{
		AppID:         42,
		AppPrivateKey: testKey(t),
		PAT:           pat,
		APIVersion:    "2026-03-10",
		Timeout:       5 * time.Second,
		Logger:        zap.NewNop(),
		Metrics:       m,
		BaseURL:       f.srv.URL,
		Sleep:         s.sleep,
		Now:           func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshInstallations(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &testEnv{fake: f, client: c, metrics: m, sleeps: s}
}
