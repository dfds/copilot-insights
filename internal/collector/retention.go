package collector

import (
	"context"
	"fmt"
)

func (c *Collectors) Retention(ctx context.Context) (Outcome, error) {
	cutoff := c.today().AddDate(0, 0, -c.cfg.RawDays)
	n, err := c.st.PruneRawReports(ctx, cutoff)
	if err != nil {
		return Outcome{}, fmt.Errorf("prune raw reports: %w", err)
	}
	return Outcome{Items: int(n)}, nil
}
