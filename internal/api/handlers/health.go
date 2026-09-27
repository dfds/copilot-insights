package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type ReadyChecker interface {
	Ready(ctx context.Context) error
}

// Health exposes liveness and readiness.
type Health struct {
	ready ReadyChecker
}

func NewHealth(ready ReadyChecker) *Health {
	return &Health{ready: ready}
}

func (h *Health) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Health) Readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.ready.Ready(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ready"})
}
