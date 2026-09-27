package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

// Seats snapshots Copilot seats and the org billing summary. Every active org
// is probed through /copilot/billing, including orgs marked
// copilot_enabled=false, so this collector is the one that flips the flag in
// either direction. Enterprise seats need the PAT and are skipped without it.
func (c *Collectors) Seats(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	scopes, err := c.st.ActiveScopes(ctx)
	if err != nil {
		return out, fmt.Errorf("load scopes: %w", err)
	}
	day := c.today()

	var targets []store.ScopeState
	for _, sc := range scopes {
		if sc.Scope.Kind == gh.ScopeEnterprise && !c.gh.HasPAT() {
			if !c.warnedNoPAT.Swap(true) {
				c.logger.Warn("CPI_GITHUB_PAT is not set; enterprise seats are not collected")
			}
			continue
		}
		targets = append(targets, sc)
	}
	out.Scopes = len(targets)
	items := make([]int, len(targets))
	errs := make([]error, len(targets))
	forEach(len(targets), c.cfg.Concurrency, func(i int) {
		if targets[i].Scope.Kind == gh.ScopeOrg {
			items[i], errs[i] = c.orgSeats(ctx, targets[i], day)
		} else {
			items[i], errs[i] = c.saveSeats(ctx, targets[i].Scope, day, -1)
		}
	})
	for i, sc := range targets {
		if errs[i] != nil {
			out.fail(sc.Scope.String(), errs[i])
		}
		out.Items += items[i]
	}
	return out, nil
}

func (c *Collectors) orgSeats(ctx context.Context, sc store.ScopeState, day time.Time) (int, error) {
	org := sc.Scope.Name
	b, err := c.gh.CopilotBilling(ctx, org)
	if errors.Is(err, gh.ErrNotFound) {
		return 0, c.setCopilot(ctx, sc, false)
	}
	if err != nil {
		return 0, fmt.Errorf("copilot billing: %w", err)
	}
	if err := c.setCopilot(ctx, sc, true); err != nil {
		return 0, err
	}
	if err := c.st.SaveCopilotBilling(ctx, org, day, b); err != nil {
		return 0, fmt.Errorf("save copilot billing: %w", err)
	}
	return c.saveSeats(ctx, sc.Scope, day, b.SeatBreakdown.Total)
}

// saveSeats fetches and stores a scope's seats. expected is the org billing
// seat total, or -1 when unknown. An empty listing replaces the stored seats
// only when billing confirms zero seats; otherwise it is treated as a bad
// response and the current rows are kept.
func (c *Collectors) saveSeats(ctx context.Context, scope gh.Scope, day time.Time, expected int) (int, error) {
	seats, err := c.gh.Seats(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("seats: %w", err)
	}
	if len(seats) == 0 && expected != 0 {
		c.logger.Warn("seat listing is empty; keeping the stored seats",
			zap.String("scope", scope.String()), zap.Int("billingSeatTotal", expected))
		return 0, nil
	}
	seats = mergeSeats(seats)
	if err := c.st.SaveSeats(ctx, scope, day, seats); err != nil {
		return 0, fmt.Errorf("save seats: %w", err)
	}
	return len(seats), nil
}

// mergeSeats returns one seat per user, in first-seen order. The enterprise
// listing has one entry per org seat, so a user seated in two orgs appears
// twice, while the seat tables key on user_id. The most recently active entry
// is kept (ties: lower org name), with the earliest CreatedAt of the group.
func mergeSeats(seats []gh.Seat) []gh.Seat {
	idx := make(map[int64]int, len(seats))
	out := make([]gh.Seat, 0, len(seats))
	for _, s := range seats {
		i, dup := idx[s.UserID]
		if !dup {
			idx[s.UserID] = len(out)
			out = append(out, s)
			continue
		}
		kept := out[i]
		if moreRecent(s, kept) {
			kept, s = s, kept
		}
		if s.CreatedAt != nil && (kept.CreatedAt == nil || s.CreatedAt.Before(*kept.CreatedAt)) {
			kept.CreatedAt = s.CreatedAt
		}
		out[i] = kept
	}
	return out
}

func moreRecent(a, b gh.Seat) bool {
	switch {
	case a.LastActivityAt == nil && b.LastActivityAt == nil:
		return a.OrgName < b.OrgName
	case b.LastActivityAt == nil:
		return true
	case a.LastActivityAt == nil:
		return false
	case !a.LastActivityAt.Equal(*b.LastActivityAt):
		return a.LastActivityAt.After(*b.LastActivityAt)
	default:
		return a.OrgName < b.OrgName
	}
}

func (c *Collectors) setCopilot(ctx context.Context, sc store.ScopeState, enabled bool) error {
	if sc.CopilotEnabled == enabled {
		return nil
	}
	if err := c.st.SetCopilotEnabled(ctx, sc.Scope, enabled); err != nil {
		return fmt.Errorf("set copilot_enabled: %w", err)
	}
	if enabled {
		c.logger.Info("Copilot endpoints answer again; scope re-enabled", zap.String("scope", sc.Scope.String()))
	} else {
		c.logger.Info("Copilot endpoints return 404; scope skipped by the other collectors until they answer",
			zap.String("scope", sc.Scope.String()))
	}
	return nil
}
