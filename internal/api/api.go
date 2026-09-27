package api

import (
	"time"

	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
	"go.dfds.cloud/copilot-insights/internal/api/handlers"
)

type Options struct {
	Ready      handlers.ReadyChecker
	Store      handlers.Store
	Collectors handlers.Collectors
	Enterprise string
	// AuthMW guards /api/v1 (the OIDC middleware, or a pass-through when OIDC
	// is disabled). AdminMW additionally guards /api/v1/admin.
	AuthMW  gin.HandlerFunc
	AdminMW gin.HandlerFunc
	Now     func() time.Time
}

// Configure registers all routes on the given router. /healthz and /readyz
// stay unauthenticated.
func Configure(router *gin.Engine, o Options) *gin.RouterGroup {
	health := handlers.NewHealth(o.Ready)
	router.GET("/healthz", health.Healthz)
	router.GET("/readyz", health.Readyz)

	v1 := router.Group("/api/v1")
	v1.Use(gzip.Gzip(gzip.BestSpeed))
	if o.AuthMW != nil {
		v1.Use(o.AuthMW)
	}

	h := handlers.NewAPI(o.Store, o.Collectors, o.Enterprise, o.Now)
	v1.GET("/scopes", h.ListScopes)
	v1.GET("/status", h.Status)
	v1.GET("/summary", h.Summary)
	v1.GET("/reports/organization/daily", h.OrganizationDaily)
	v1.GET("/reports/organization/28d", h.Organization28d)
	v1.GET("/reports/users/daily", h.UsersDaily)
	v1.GET("/reports/users/totals", h.UsersTotals)
	v1.GET("/reports/repos/daily", h.ReposDaily)
	v1.GET("/reports/repos/totals", h.ReposTotals)
	v1.GET("/teams/daily", h.TeamsDaily)
	v1.GET("/billing/summary", h.BillingSummary)
	v1.GET("/billing/usage", h.BillingUsage)
	v1.GET("/billing/premium-requests", h.PremiumRequests)
	v1.GET("/credits/usage", h.CreditsUsage)
	v1.GET("/credits/hourly", h.CreditsHourly)
	v1.GET("/budgets", h.Budgets)
	v1.GET("/seats", h.ListSeats)
	v1.GET("/seats/history", h.SeatHistory)
	v1.GET("/identity/users", h.ListIdentities)
	v1.GET("/users/by-email", h.GetUserByEmail)
	v1.GET("/users/by-email/:email", h.GetUserByEmail)
	v1.GET("/users/:login", h.GetUser)

	admin := v1.Group("/admin")
	if o.AdminMW != nil {
		admin.Use(o.AdminMW)
	}
	admin.POST("/collect/:collector", h.TriggerCollector)
	return v1
}
