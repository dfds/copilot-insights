package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.dfds.cloud/copilot-insights/internal/collector"
	"go.dfds.cloud/copilot-insights/internal/store"
)

// ListScopes returns every scope, including inactive ones.
func (a *API) ListScopes(c *gin.Context) {
	ctx := c.Request.Context()
	scopes, err := a.st.Scopes(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	meta, err := a.meta(ctx, "members")
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, scopes, meta)
}

// Status returns the latest run of each registered collector, followed by
// collectors that have runs recorded but are no longer registered.
func (a *API) Status(c *gin.Context) {
	ctx := c.Request.Context()
	recorded, err := a.st.CollectorStatuses(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	byName := make(map[string]store.CollectorStatus, len(recorded))
	for _, st := range recorded {
		byName[st.Collector] = st
	}
	out := make([]store.CollectorStatus, 0, len(recorded))
	registered := map[string]bool{}
	for _, name := range a.collectors.Names() {
		registered[name] = true
		st, ok := byName[name]
		if !ok {
			st = store.CollectorStatus{Collector: name}
		}
		out = append(out, st)
	}
	for _, st := range recorded {
		if !registered[st.Collector] {
			out = append(out, st)
		}
	}
	meta, err := a.meta(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	respond(c, out, meta)
}

// TriggerCollector starts a collector run in the background: 202 when started,
// 409 when it is already running, 404 for an unknown collector.
func (a *API) TriggerCollector(c *gin.Context) {
	name := c.Param("collector")
	switch err := a.collectors.Trigger(name); {
	case err == nil:
		c.JSON(http.StatusAccepted, gin.H{"data": gin.H{"collector": name, "status": "started"}})
	case errors.Is(err, collector.ErrBusy):
		c.JSON(http.StatusConflict, gin.H{"error": "busy", "detail": "collector " + name + " is already running"})
	case errors.Is(err, collector.ErrUnknownCollector):
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown_collector", "collectors": a.collectors.Names()})
	default:
		fail(c, err)
	}
}
