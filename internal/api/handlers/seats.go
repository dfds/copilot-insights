package handlers

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.dfds.cloud/copilot-insights/internal/store"
)

// ListSeats returns a scope's current seats: ?scope=&login=&inactiveDays=
// with limit/offset paging. inactiveDays=N keeps seats with no activity in
// the last N days, never-used seats included.
func (a *API) ListSeats(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	p, err := page(c)
	if err != nil {
		fail(c, err)
		return
	}
	f := store.SeatFilter{Scope: sc, Login: c.Query("login"), Page: p}
	if raw := c.Query("inactiveDays"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRangeDays {
			fail(c, badRequest("invalid_inactive_days", "inactiveDays must be between 1 and "+strconv.Itoa(maxRangeDays)))
			return
		}
		f.InactiveSince = a.now().Add(-time.Duration(n) * 24 * time.Hour)
	}
	seats, total, err := a.st.Seats(ctx, f)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "seats", "members")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	meta.Page = pageMeta(p, total)
	respond(c, seats, meta)
}

// SeatHistory returns daily seat counts for a scope over ?from=&to=.
func (a *API) SeatHistory(c *gin.Context) {
	ctx := c.Request.Context()
	sc, err := a.scope(c)
	if err != nil {
		fail(c, err)
		return
	}
	from, to, err := a.dateRange(c)
	if err != nil {
		fail(c, err)
		return
	}
	days, err := a.st.SeatHistory(ctx, sc, from, to)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "seats")
	if err != nil {
		fail(c, err)
		return
	}
	meta.Scope = sc.String()
	setRange(meta, from, to)
	respond(c, days, meta)
}
