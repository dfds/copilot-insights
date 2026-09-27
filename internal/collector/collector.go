package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/report"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

type GitHub interface {
	HasPAT() bool
	RefreshInstallations(ctx context.Context) ([]gh.Installation, error)
	OrgMembers(ctx context.Context, org string) ([]gh.Member, error)
	Seats(ctx context.Context, scope gh.Scope) ([]gh.Seat, error)
	CopilotBilling(ctx context.Context, org string) (*gh.CopilotBilling, error)
	DailyReport(ctx context.Context, scope gh.Scope, t gh.ReportType, day time.Time) (*gh.Report, error)
	LatestReport(ctx context.Context, scope gh.Scope, t gh.ReportType) (*gh.Report, error)
	BillingUsage(ctx context.Context, enterprise string, year, month int) (*gh.BillingUsageResponse, error)
	AICreditReport(ctx context.Context, enterprise string, start, end time.Time) ([]gh.AICreditRow, error)
	PremiumRequestUsage(ctx context.Context, scope gh.Scope, q gh.PremiumRequestQuery) (*gh.PremiumRequestUsage, error)
	Budgets(ctx context.Context, enterprise string) ([]gh.Budget, error)
	BudgetUserStates(ctx context.Context, enterprise, budgetID string) ([]gh.BudgetUserState, error)
}

type Store interface {
	SyncScopes(ctx context.Context, scopes []store.ScopeState) error
	ActiveScopes(ctx context.Context) ([]store.ScopeState, error)
	SetCopilotEnabled(ctx context.Context, scope gh.Scope, enabled bool) error
	ReplaceOrgMembers(ctx context.Context, members map[string][]store.OrgMember) (int, error)
	SaveSeats(ctx context.Context, scope gh.Scope, day time.Time, seats []gh.Seat) error
	SaveCopilotBilling(ctx context.Context, org string, day time.Time, b *gh.CopilotBilling) error
	PruneRawReports(ctx context.Context, cutoff time.Time) (int64, error)
	ReportStatuses(ctx context.Context, scope gh.Scope, reportType string, from, to time.Time) (map[string]string, error)
	SaveReportNoData(ctx context.Context, scope gh.Scope, reportType string, day time.Time) error
	SaveReport(ctx context.Context, scope gh.Scope, reportType string, day time.Time, rows []json.RawMessage, p *report.Parsed) error
	SaveBillingUsage(ctx context.Context, enterprise string, items []gh.BillingUsageItem) (int, error)
	ReplaceAICredits(ctx context.Context, enterprise string, from, to time.Time, rows []gh.AICreditRow, settle bool) (int, error)
	SettledAICreditMonths(ctx context.Context, enterprise string) (map[string]bool, error)
	LegacyPRUFetched(ctx context.Context, enterprise string) (map[store.LegacyPRUKey]bool, error)
	SaveLegacyPRU(ctx context.Context, enterprise string, k store.LegacyPRUKey, items []gh.PremiumRequestUsageItem) error
	UserLogins(ctx context.Context) ([]string, error)
	ReplaceBudgets(ctx context.Context, enterprise string, budgets []gh.Budget, states map[string][]gh.BudgetUserState) (int, error)
}

type Config struct {
	Enterprise  string
	OrgExclude  []string
	Concurrency int
	RawDays     int
	// Reports: days before today to stop at, and days to collect back from there.
	ReportLagDays int
	BackfillDays  int
	TeamsEnabled  bool
	// BillingLagDays: days before today the ai_credit month to date ends.
	BillingLagDays int
	Now            func() time.Time // test hook
}

type Collectors struct {
	gh     GitHub
	st     Store
	cfg    Config
	logger *zap.Logger

	warnedNoPAT     atomic.Bool
	warnedNoInstall atomic.Bool

	installMu     sync.Mutex
	installLoaded atomic.Bool
}

// ensureInstallations loads the App installations into the client unless the
// members collector already has. The client resolves org credentials from
// them, so a collector that runs before members (RUNONSTART=false, or a
// manual trigger) would otherwise get ErrNoInstallation for every org.
func (c *Collectors) ensureInstallations(ctx context.Context) error {
	if c.installLoaded.Load() {
		return nil
	}
	c.installMu.Lock()
	defer c.installMu.Unlock()
	if c.installLoaded.Load() {
		return nil
	}
	if _, err := c.gh.RefreshInstallations(ctx); err != nil {
		return fmt.Errorf("list installations: %w", err)
	}
	c.installLoaded.Store(true)
	return nil
}

func New(client GitHub, st Store, cfg Config, logger *zap.Logger) *Collectors {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	return &Collectors{gh: client, st: st, cfg: cfg, logger: logger}
}

func (c *Collectors) excluded(org string) bool {
	for _, x := range c.cfg.OrgExclude {
		if strings.EqualFold(x, org) {
			return true
		}
	}
	return false
}

// Snapshot dates use UTC calendar days.
func (c *Collectors) today() time.Time {
	return c.cfg.Now().UTC().Truncate(24 * time.Hour)
}

// forEach calls fn for 0..n-1 with at most limit calls in flight and returns
// when all have finished. fn must stop promptly once ctx is cancelled.
func forEach(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}
