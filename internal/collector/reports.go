package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/report"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

const (
	// noDataRetryDays: a day stored as no_data is fetched again while it is
	// this recent, because GitHub may not have generated it yet.
	noDataRetryDays = 3
	// maxReportAgeDays is the oldest day the 1-day reports accept; older days
	// answer 400 "Date must be within the last year".
	maxReportAgeDays = 364
	// maxConsecutiveFailures stops a report's backfill after this many failed
	// days in a row, so a missing permission costs a few calls, not one per day.
	maxConsecutiveFailures = 3
)

type reportTask struct {
	scope  gh.Scope
	typ    gh.ReportType
	latest bool
}

func (t reportTask) String() string {
	w := gh.Window1Day
	if t.latest {
		w = gh.Window28Day
	}
	return t.scope.String() + " " + t.typ.APIName(t.scope.Kind, w)
}

// Reports collects the 1-day usage metrics reports (summary, users, repos,
// and user-teams on orgs when TeamsEnabled) for every day in the backfill
// window, plus the latest 28-day summary for its feature engagement block.
// A day stored as complete is never fetched again.
func (c *Collectors) Reports(ctx context.Context) (Outcome, error) {
	var out Outcome
	if err := c.ensureInstallations(ctx); err != nil {
		return out, err
	}
	scopes, err := c.st.ActiveScopes(ctx)
	if err != nil {
		return out, fmt.Errorf("load scopes: %w", err)
	}

	var tasks []reportTask
	for _, sc := range scopes {
		if !sc.CopilotEnabled {
			continue
		}
		if sc.InstallationID == 0 {
			if !c.warnedNoInstall.Swap(true) {
				c.logger.Warn("the App is not installed on the scope; its reports are not collected",
					zap.String("scope", sc.Scope.String()))
			}
			continue
		}
		types := []gh.ReportType{gh.ReportSummary, gh.ReportUsers, gh.ReportRepos}
		if sc.Scope.Kind == gh.ScopeOrg && c.cfg.TeamsEnabled {
			types = append(types, gh.ReportUserTeams)
		}
		for _, t := range types {
			tasks = append(tasks, reportTask{scope: sc.Scope, typ: t})
		}
		tasks = append(tasks, reportTask{scope: sc.Scope, typ: gh.ReportSummary, latest: true})
	}

	out.Scopes = len(tasks)
	items := make([]int, len(tasks))
	errs := make([]error, len(tasks))
	forEach(len(tasks), c.cfg.Concurrency, func(i int) {
		if tasks[i].latest {
			items[i], errs[i] = c.latestReport(ctx, tasks[i])
		} else {
			items[i], errs[i] = c.dailyReports(ctx, tasks[i])
		}
	})
	for i, t := range tasks {
		if errs[i] != nil {
			out.fail(t.String(), errs[i])
		}
		out.Items += items[i]
	}
	return out, nil
}

func (c *Collectors) reportWindow() (first, last, retryFrom time.Time) {
	today := c.today()
	last = today.AddDate(0, 0, -c.cfg.ReportLagDays)
	first = last.AddDate(0, 0, 1-max(c.cfg.BackfillDays, 1))
	if oldest := today.AddDate(0, 0, -maxReportAgeDays); first.Before(oldest) {
		first = oldest
	}
	return first, last, today.AddDate(0, 0, -noDataRetryDays)
}

// dailyReports fetches the task's missing days, newest first, so an
// interrupted first backfill has the most recent days.
func (c *Collectors) dailyReports(ctx context.Context, t reportTask) (int, error) {
	name := t.typ.APIName(t.scope.Kind, gh.Window1Day)
	first, last, retryFrom := c.reportWindow()
	if last.Before(first) {
		return 0, nil
	}
	have, err := c.st.ReportStatuses(ctx, t.scope, name, first, last)
	if err != nil {
		return 0, fmt.Errorf("load report status: %w", err)
	}

	items, failed, streak := 0, 0, 0
	var firstErr error
	for day := last; !day.Before(first); day = day.AddDate(0, 0, -1) {
		switch have[day.Format(time.DateOnly)] {
		case store.ReportComplete:
			continue
		case store.ReportNoData:
			if day.Before(retryFrom) {
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return items, err
		}
		err := c.fetchDay(ctx, t, name, day)
		if err == nil {
			items++
			streak = 0
			continue
		}
		if errors.Is(err, gh.ErrNotFound) && !day.Before(retryFrom) {
			c.logger.Debug("report not available yet", zap.String("report", t.String()),
				zap.String("day", day.Format(time.DateOnly)))
			continue
		}
		failed++
		streak++
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", day.Format(time.DateOnly), err)
		}
		if streak >= maxConsecutiveFailures {
			return items, fmt.Errorf("stopped after %d consecutive failures; first: %w", streak, firstErr)
		}
	}
	if firstErr != nil {
		return items, fmt.Errorf("%d days failed; first: %w", failed, firstErr)
	}
	return items, nil
}

func (c *Collectors) fetchDay(ctx context.Context, t reportTask, name string, day time.Time) error {
	r, err := c.gh.DailyReport(ctx, t.scope, t.typ, day)
	if err != nil {
		return err
	}
	if r.NoData {
		return c.st.SaveReportNoData(ctx, t.scope, name, day)
	}
	p, err := report.Parse(t.typ, gh.Window1Day, r.Rows)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if err := c.st.SaveReport(ctx, t.scope, name, day, r.Rows, p); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

// latestReport stores the feature engagement block of the latest 28-day
// summary under its end day. It is fetched every run; the upsert replaces the
// previous copy for that day.
func (c *Collectors) latestReport(ctx context.Context, t reportTask) (int, error) {
	r, err := c.gh.LatestReport(ctx, t.scope, t.typ)
	if err != nil {
		return 0, err
	}
	if r.NoData {
		return 0, nil
	}
	p, err := report.Parse(t.typ, gh.Window28Day, r.Rows)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	end, err := time.Parse(time.DateOnly, p.EndDay)
	if err != nil {
		return 0, fmt.Errorf("report_end_day %q: %w", p.EndDay, err)
	}
	if err := c.st.SaveReport(ctx, t.scope, t.typ.APIName(t.scope.Kind, gh.Window28Day), end, r.Rows, p); err != nil {
		return 0, fmt.Errorf("save: %w", err)
	}
	return 1, nil
}
