package collector

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/metrics"
	"go.dfds.cloud/copilot-insights/internal/report"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

var notFound = &gh.StatusError{Method: http.MethodGet, Path: "/x", Status: http.StatusNotFound}

type fakeGitHub struct {
	mu sync.Mutex

	pat           bool
	installations []gh.Installation
	installErr    error
	members       map[string][]gh.Member
	memberErr     map[string]error
	seats         map[string][]gh.Seat // key: Scope.String()
	seatErr       map[string]error
	billing       map[string]*gh.CopilotBilling
	billingErr    map[string]error
	// reports and reportErr are keyed "<scope> <type> <day>"; a missing key
	// answers with no data. latest is keyed "<scope> <type>".
	reports   map[string]*gh.Report
	reportErr map[string]error
	latest    map[string]*gh.Report
	// usage is keyed "<year>-<month>"; aiCredits and aiCreditErr are keyed
	// "<start>..<end>"; pru is keyed "<year>-<month> <org> <login>".
	usage       map[string]*gh.BillingUsageResponse
	usageErr    error
	aiCredits   map[string][]gh.AICreditRow
	aiCreditErr map[string]error
	pru         map[string][]gh.PremiumRequestUsageItem
	pruErr      func(q gh.PremiumRequestQuery) error
	// budgetStates and budgetStateErr are keyed by budget id.
	budgets        []gh.Budget
	budgetErr      error
	budgetStates   map[string][]gh.BudgetUserState
	budgetStateErr map[string]error

	calls []string
}

func (f *fakeGitHub) Budgets(context.Context, string) ([]gh.Budget, error) {
	f.record("budgets")
	return f.budgets, f.budgetErr
}

func (f *fakeGitHub) BudgetUserStates(_ context.Context, _, id string) ([]gh.BudgetUserState, error) {
	f.record("budget states " + id)
	return f.budgetStates[id], f.budgetStateErr[id]
}

func (f *fakeGitHub) BillingUsage(_ context.Context, _ string, year, month int) (*gh.BillingUsageResponse, error) {
	key := strconv.Itoa(year) + "-" + strconv.Itoa(month)
	f.record("usage " + key)
	if f.usageErr != nil {
		return nil, f.usageErr
	}
	if r := f.usage[key]; r != nil {
		return r, nil
	}
	return &gh.BillingUsageResponse{}, nil
}

func (f *fakeGitHub) AICreditReport(_ context.Context, _ string, start, end time.Time) ([]gh.AICreditRow, error) {
	key := start.Format(time.DateOnly) + ".." + end.Format(time.DateOnly)
	f.record("aicredit " + key)
	if err := f.aiCreditErr[key]; err != nil {
		return nil, err
	}
	return f.aiCredits[key], nil
}

func (f *fakeGitHub) PremiumRequestUsage(_ context.Context, _ gh.Scope, q gh.PremiumRequestQuery) (*gh.PremiumRequestUsage, error) {
	key := fmt.Sprintf("%d-%d %s %s", q.Year, q.Month, q.Organization, q.User)
	f.record("pru " + key)
	if f.pruErr != nil {
		if err := f.pruErr(q); err != nil {
			return nil, err
		}
	}
	return &gh.PremiumRequestUsage{UsageItems: f.pru[key]}, nil
}

func (f *fakeGitHub) DailyReport(_ context.Context, scope gh.Scope, t gh.ReportType, day time.Time) (*gh.Report, error) {
	key := scope.String() + " " + string(t) + " " + day.Format(time.DateOnly)
	f.record("report " + key)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.reportErr[key]; err != nil {
		return nil, err
	}
	if r := f.reports[key]; r != nil {
		return r, nil
	}
	return &gh.Report{NoData: true}, nil
}

func (f *fakeGitHub) LatestReport(_ context.Context, scope gh.Scope, t gh.ReportType) (*gh.Report, error) {
	key := scope.String() + " " + string(t)
	f.record("latest " + key)
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.latest[key]; r != nil {
		return r, nil
	}
	return &gh.Report{NoData: true}, nil
}

func (f *fakeGitHub) reportCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if k, ok := strings.CutPrefix(c, "report "); ok {
			out = append(out, k)
		}
	}
	return out
}

func (f *fakeGitHub) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeGitHub) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, call)
}

func (f *fakeGitHub) HasPAT() bool { return f.pat }

func (f *fakeGitHub) RefreshInstallations(context.Context) ([]gh.Installation, error) {
	f.record("installations")
	return f.installations, f.installErr
}

func (f *fakeGitHub) OrgMembers(_ context.Context, org string) ([]gh.Member, error) {
	f.record("members " + org)
	return f.members[org], f.memberErr[org]
}

func (f *fakeGitHub) Seats(_ context.Context, scope gh.Scope) ([]gh.Seat, error) {
	f.record("seats " + scope.String())
	return f.seats[scope.String()], f.seatErr[scope.String()]
}

func (f *fakeGitHub) CopilotBilling(_ context.Context, org string) (*gh.CopilotBilling, error) {
	f.record("billing " + org)
	if err := f.billingErr[org]; err != nil {
		return nil, err
	}
	return f.billing[org], nil
}

// fakeStore keeps state in maps with the same replace semantics as the
// Postgres store, at org / scope granularity.
type fakeStore struct {
	mu sync.Mutex

	scopes    map[gh.Scope]store.ScopeState
	members   map[string][]store.OrgMember
	replaced  [][]string
	seats     map[gh.Scope][]gh.Seat
	seatSaves int
	billing   map[string]*gh.CopilotBilling
	cutoff    time.Time
	pruned    int64
	// reports is keyed "<scope> <reportType> <day>" with the stored status.
	reports map[string]string
	parsed  map[string]*report.Parsed
	saveErr error

	usage []gh.BillingUsageItem
	// credits is keyed "<from>..<to>" with the settle flag appended as "!".
	credits map[string][]gh.AICreditRow
	settled map[string]bool
	pru     map[store.LegacyPRUKey][]gh.PremiumRequestUsageItem
	logins  []string

	budgets      []gh.Budget
	budgetStates map[string][]gh.BudgetUserState
	budgetSaves  int
}

func (s *fakeStore) ReplaceBudgets(_ context.Context, _ string, budgets []gh.Budget, states map[string][]gh.BudgetUserState) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	s.budgets, s.budgetStates = budgets, states
	s.budgetSaves++
	n := len(budgets)
	for _, ss := range states {
		n += len(ss)
	}
	return n, nil
}

func (s *fakeStore) SaveBillingUsage(_ context.Context, _ string, items []gh.BillingUsageItem) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	s.usage = append(s.usage, items...)
	return len(items), nil
}

func (s *fakeStore) ReplaceAICredits(_ context.Context, _ string, from, to time.Time, rows []gh.AICreditRow, settle bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return 0, s.saveErr
	}
	key := from.Format(time.DateOnly) + ".." + to.Format(time.DateOnly)
	if settle {
		key += "!"
		s.settled[from.Format("2006-01")] = true
	}
	s.credits[key] = rows
	return len(rows), nil
}

func (s *fakeStore) SettledAICreditMonths(context.Context, string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.settled), nil
}

func (s *fakeStore) LegacyPRUFetched(context.Context, string) (map[store.LegacyPRUKey]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[store.LegacyPRUKey]bool{}
	for k := range s.pru {
		out[k] = true
	}
	return out, nil
}

func (s *fakeStore) SaveLegacyPRU(_ context.Context, _ string, k store.LegacyPRUKey, items []gh.PremiumRequestUsageItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.pru[k] = items
	return nil
}

func (s *fakeStore) UserLogins(context.Context) ([]string, error) {
	return s.logins, nil
}

func reportKey(scope gh.Scope, reportType string, day time.Time) string {
	return scope.String() + " " + reportType + " " + day.Format(time.DateOnly)
}

func (s *fakeStore) ReportStatuses(_ context.Context, scope gh.Scope, reportType string, from, to time.Time) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if st, ok := s.reports[reportKey(scope, reportType, d)]; ok {
			out[d.Format(time.DateOnly)] = st
		}
	}
	return out, nil
}

func (s *fakeStore) SaveReportNoData(_ context.Context, scope gh.Scope, reportType string, day time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports[reportKey(scope, reportType, day)] = store.ReportNoData
	return nil
}

func (s *fakeStore) SaveReport(_ context.Context, scope gh.Scope, reportType string, day time.Time, _ []json.RawMessage, p *report.Parsed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.reports[reportKey(scope, reportType, day)] = store.ReportComplete
	s.parsed[reportKey(scope, reportType, day)] = p
	return nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		scopes:  map[gh.Scope]store.ScopeState{},
		members: map[string][]store.OrgMember{},
		seats:   map[gh.Scope][]gh.Seat{},
		billing: map[string]*gh.CopilotBilling{},
		reports: map[string]string{},
		parsed:  map[string]*report.Parsed{},
		credits: map[string][]gh.AICreditRow{},
		settled: map[string]bool{},
		pru:     map[store.LegacyPRUKey][]gh.PremiumRequestUsageItem{},
	}
}

func (s *fakeStore) SyncScopes(_ context.Context, scopes []store.ScopeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.scopes {
		v.Active = false
		s.scopes[k] = v
	}
	for _, sc := range scopes {
		old, ok := s.scopes[sc.Scope]
		if !ok {
			old.CopilotEnabled = true
		}
		old.Scope, old.InstallationID, old.Active = sc.Scope, sc.InstallationID, true
		s.scopes[sc.Scope] = old
	}
	return nil
}

func (s *fakeStore) ActiveScopes(context.Context) ([]store.ScopeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.ScopeState
	for _, v := range s.scopes {
		if v.Active {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b store.ScopeState) int {
		return cmp.Or(cmp.Compare(a.Scope.Kind, b.Scope.Kind), cmp.Compare(a.Scope.Name, b.Scope.Name))
	})
	return out, nil
}

func (s *fakeStore) SetCopilotEnabled(_ context.Context, scope gh.Scope, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.scopes[scope]
	v.CopilotEnabled = enabled
	s.scopes[scope] = v
	return nil
}

func (s *fakeStore) ReplaceOrgMembers(_ context.Context, members map[string][]store.OrgMember) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for org, list := range members {
		s.members[org] = list
		n += len(list)
	}
	s.replaced = append(s.replaced, slices.Sorted(maps.Keys(members)))
	return n, nil
}

func (s *fakeStore) SaveSeats(_ context.Context, scope gh.Scope, _ time.Time, seats []gh.Seat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seats[scope] = seats
	s.seatSaves++
	return nil
}

func (s *fakeStore) SaveCopilotBilling(_ context.Context, org string, _ time.Time, b *gh.CopilotBilling) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.billing[org] = b
	return nil
}

func (s *fakeStore) PruneRawReports(_ context.Context, cutoff time.Time) (int64, error) {
	s.cutoff = cutoff
	return s.pruned, nil
}

type fakeRun struct {
	collector string
	status    store.RunStatus
	items     int
	errText   string
	finished  bool
}

type fakeRuns struct {
	mu   sync.Mutex
	runs []*fakeRun
}

func (r *fakeRuns) StartRun(_ context.Context, collector string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, &fakeRun{collector: collector})
	return int64(len(r.runs)), nil
}

func (r *fakeRuns) FinishRun(ctx context.Context, id int64, status store.RunStatus, items int, errText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.runs[id-1]
	run.status, run.items, run.errText, run.finished = status, items, errText, true
	return nil
}

func (r *fakeRuns) all() []fakeRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]fakeRun, len(r.runs))
	for i, x := range r.runs {
		out[i] = *x
	}
	return out
}

var testNow = time.Date(2026, 9, 23, 14, 30, 0, 0, time.UTC)

func newCollectors(t *testing.T, g *fakeGitHub, s *fakeStore) *Collectors {
	t.Helper()
	return New(g, s, Config{
		Enterprise:  "dfds",
		Concurrency: 2,
		RawDays:     400,
		Now:         func() time.Time { return testNow },
	}, zap.NewNop())
}

func newTestMetrics() *metrics.Metrics {
	return metrics.NewMetricsWith(prometheus.NewRegistry())
}
