package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

// billingProduct is the only product kept from the enterprise usage report,
// which also lists Actions, Packages and the other metered products.
const billingProduct = "copilot"

// aiCreditStart is the first day billed in AI credits. Earlier months were
// billed in premium requests, which PremiumLegacy collects.
var aiCreditStart = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

// legacyPRUFirst and legacyPRULast bound the months the premium request
// backfill requests.
var (
	legacyPRUFirst = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	legacyPRULast  = time.Date(2026, time.May, 1, 0, 0, 0, 0, time.UTC)
)

// legacyPRUGiveUp stops a premium request run when this many requests have
// failed and none has succeeded, so a retired endpoint costs a few calls a
// day rather than one per user and month.
const legacyPRUGiveUp = 10

func firstOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Billing stores the Copilot items of the enterprise billing usage report for
// the current year, and in January also for the previous December, so the
// month-to-date figures and late corrections to earlier months are picked up
// on every run.
func (c *Collectors) Billing(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	ent := gh.Enterprise(c.cfg.Enterprise)
	today := c.today()
	type period struct{ year, month int }
	periods := []period{{today.Year(), 0}}
	if today.Month() == time.January {
		periods = append(periods, period{today.Year() - 1, 12})
	}
	out.Scopes = len(periods)
	for _, p := range periods {
		label := fmt.Sprintf("%s billing usage %d", ent, p.year)
		if p.month > 0 {
			label += fmt.Sprintf("-%02d", p.month)
		}
		resp, err := c.gh.BillingUsage(ctx, c.cfg.Enterprise, p.year, p.month)
		if err != nil {
			out.fail(label, err)
			continue
		}
		var items []gh.BillingUsageItem
		for _, it := range resp.UsageItems {
			if strings.EqualFold(it.Product, billingProduct) {
				items = append(items, it)
			}
		}
		n, err := c.st.SaveBillingUsage(ctx, c.cfg.Enterprise, items)
		if err != nil {
			out.fail(label, fmt.Errorf("save: %w", err))
			continue
		}
		out.Items += n
	}
	return out, nil
}

type creditWindow struct {
	from, to time.Time
	settle   bool
}

func (w creditWindow) String() string {
	return w.from.Format(time.DateOnly) + ".." + w.to.Format(time.DateOnly)
}

// AICredits collects the enterprise ai_credit billing report. Every run
// replaces the month to date, ending BillingLagDays before today. A closed
// month is requested once more in full from the second day of the next
// month, when its last day has settled, and is then not requested again.
// Closed months are backfilled the same way from the later of aiCreditStart
// and the month BackfillDays ago.
func (c *Collectors) AICredits(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	windows, err := c.creditWindows(ctx)
	if err != nil {
		return out, err
	}
	out.Scopes = len(windows)
	ent := gh.Enterprise(c.cfg.Enterprise)
	for _, w := range windows {
		label := fmt.Sprintf("%s ai_credit %s", ent, w)
		rows, err := c.gh.AICreditReport(ctx, c.cfg.Enterprise, w.from, w.to)
		if err != nil {
			out.fail(label, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if len(rows) == 0 {
			// An empty report would delete the stored rows; a month to date can
			// be empty on its first day, a closed month should not be.
			if w.settle {
				out.fail(label, errors.New("the report for a closed month has no rows"))
			} else {
				c.logger.Info("ai_credit report is empty; stored rows kept", zap.Stringer("window", w))
			}
			continue
		}
		n, err := c.st.ReplaceAICredits(ctx, c.cfg.Enterprise, w.from, w.to, rows, w.settle)
		if err != nil {
			out.fail(label, fmt.Errorf("save: %w", err))
			continue
		}
		out.Items += n
	}
	return out, nil
}

func (c *Collectors) creditWindows(ctx context.Context) ([]creditWindow, error) {
	today := c.today()
	end := today.AddDate(0, 0, -c.cfg.BillingLagDays)
	if end.Before(aiCreditStart) {
		return nil, nil
	}
	settled, err := c.st.SettledAICreditMonths(ctx, c.cfg.Enterprise)
	if err != nil {
		return nil, fmt.Errorf("load settled months: %w", err)
	}
	current := firstOfMonth(end)
	first := firstOfMonth(today.AddDate(0, 0, -c.cfg.BackfillDays))
	// The month before the current one is always settled, whatever the
	// backfill, since its month to date was collected.
	if prev := current.AddDate(0, -1, 0); prev.Before(first) {
		first = prev
	}
	if first.Before(aiCreditStart) {
		first = aiCreditStart
	}
	var windows []creditWindow
	for m := first; m.Before(current); m = m.AddDate(0, 1, 0) {
		next := m.AddDate(0, 1, 0)
		if settled[m.Format("2006-01")] || today.Before(next.AddDate(0, 0, 1)) {
			continue
		}
		windows = append(windows, creditWindow{from: m, to: next.AddDate(0, 0, -1), settle: true})
	}
	return append(windows, creditWindow{from: current, to: end}), nil
}

// PremiumLegacy backfills premium request usage for 2026-01..05, the months
// before AI credits, from the enterprise premium_request/usage endpoint: the
// enterprise total, each Copilot-enabled org, and each known member. Every
// request is made once; a request that returns no items is recorded too, so
// later runs only request members added since.
func (c *Collectors) PremiumLegacy(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	ent := c.cfg.Enterprise
	fetched, err := c.st.LegacyPRUFetched(ctx, ent)
	if err != nil {
		return out, fmt.Errorf("load fetched requests: %w", err)
	}
	scopes, err := c.st.ActiveScopes(ctx)
	if err != nil {
		return out, fmt.Errorf("load scopes: %w", err)
	}
	logins, err := c.st.UserLogins(ctx)
	if err != nil {
		return out, fmt.Errorf("load logins: %w", err)
	}
	subjects := []store.LegacyPRUKey{{}}
	for _, sc := range scopes {
		if sc.Scope.Kind == gh.ScopeOrg && sc.CopilotEnabled {
			subjects = append(subjects, store.LegacyPRUKey{Org: sc.Scope.Name})
		}
	}
	for _, l := range logins {
		subjects = append(subjects, store.LegacyPRUKey{Login: l})
	}
	var todo []store.LegacyPRUKey
	for _, s := range subjects {
		for m := legacyPRUFirst; !m.After(legacyPRULast); m = m.AddDate(0, 1, 0) {
			k := s
			k.Year, k.Month = m.Year(), int(m.Month())
			if !fetched[k] {
				todo = append(todo, k)
			}
		}
	}
	if len(todo) == 0 {
		return out, nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu        sync.Mutex
		failed    int
		succeeded int
		firstErr  error
		items     int
	)
	forEach(len(todo), c.cfg.Concurrency, func(i int) {
		if runCtx.Err() != nil {
			return
		}
		k := todo[i]
		n, err := c.legacyPRU(runCtx, k)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if runCtx.Err() != nil && ctx.Err() == nil {
				return
			}
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", legacyLabel(k), err)
			}
			if succeeded == 0 && failed >= legacyPRUGiveUp {
				cancel()
			}
			return
		}
		succeeded++
		items += n
	})
	// Failures are reported as one error so a retired endpoint does not
	// produce one line per request.
	out.Items = items
	out.Scopes = failed + succeeded
	switch {
	case failed == 0:
		if err := ctx.Err(); err != nil {
			return out, err
		}
	case succeeded == 0:
		return out, fmt.Errorf("premium request backfill: %d requests failed, none succeeded; first: %w", failed, firstErr)
	default:
		out.fail("premium request backfill", fmt.Errorf("%d of %d requests failed; first: %w", failed, failed+succeeded, firstErr))
	}
	c.logger.Info("premium request backfill", zap.Int("requests", succeeded), zap.Int("failed", failed), zap.Int("rows", items))
	return out, nil
}

func legacyLabel(k store.LegacyPRUKey) string {
	s := fmt.Sprintf("%d-%02d", k.Year, k.Month)
	switch {
	case k.Org != "":
		return s + " org " + k.Org
	case k.Login != "":
		return s + " user " + k.Login
	}
	return s + " enterprise"
}

func (c *Collectors) legacyPRU(ctx context.Context, k store.LegacyPRUKey) (int, error) {
	resp, err := c.gh.PremiumRequestUsage(ctx, gh.Enterprise(c.cfg.Enterprise), gh.PremiumRequestQuery{
		Year: k.Year, Month: k.Month, Organization: k.Org, User: k.Login,
	})
	if err != nil {
		return 0, err
	}
	if err := c.st.SaveLegacyPRU(ctx, c.cfg.Enterprise, k, resp.UsageItems); err != nil {
		return 0, fmt.Errorf("save: %w", err)
	}
	return len(resp.UsageItems), nil
}
