// Package auth validates OIDC bearer tokens and checks app roles. Verified
// roles are stored on gin.Context for route-specific authorization.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const rolesKey = "auth.roles"

// Verifier validates inbound access tokens against the configured issuer and
// audience, and authorizes on the required app role(s).
type Verifier struct {
	issuerURL     string
	audience      string
	requiredRoles []string
	log           *zap.Logger

	state atomic.Pointer[oidc.IDTokenVerifier]
}

// claims is the subset of token claims we authorize on. Azure AD places app
// roles in the `roles` claim.
type claims struct {
	Roles []string `json:"roles"`
}

// NewVerifier completes OIDC discovery before returning.
func NewVerifier(ctx context.Context, issuerURL, audience string, requiredRoles []string, log *zap.Logger) (*Verifier, error) {
	if log == nil {
		log = zap.NewNop()
	}
	v := &Verifier{
		issuerURL:     issuerURL,
		audience:      audience,
		requiredRoles: requiredRoles,
		log:           log,
	}
	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// Run blocks until ctx is done, refreshing discovery every interval. Refresh
// failures are logged; the previously loaded verifier remains in effect.
func (v *Verifier) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := v.refresh(ctx); err != nil {
				v.log.Warn("oidc discovery refresh failed", zap.Error(err))
			}
		}
	}
}

func (v *Verifier) refresh(ctx context.Context) error {
	prov, err := oidc.NewProvider(ctx, v.issuerURL)
	if err != nil {
		return fmt.Errorf("oidc discovery: %w", err)
	}
	v.state.Store(prov.Verifier(&oidc.Config{ClientID: v.audience}))
	return nil
}

func (v *Verifier) verify(ctx context.Context, rawToken string) ([]string, error) {
	verifier := v.state.Load()
	if verifier == nil {
		return nil, errors.New("oidc verifier not initialised")
	}
	tok, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("verify token: %w", err)
	}

	var c claims
	if err := tok.Claims(&c); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if !hasRequiredRole(c.Roles, v.requiredRoles) {
		return nil, fmt.Errorf("missing required role (need one of %v)", v.requiredRoles)
	}
	return c.Roles, nil
}

func hasRequiredRole(tokenRoles, requiredRoles []string) bool {
	if len(requiredRoles) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(tokenRoles))
	for _, r := range tokenRoles {
		set[r] = struct{}{}
	}
	for _, required := range requiredRoles {
		if _, ok := set[required]; ok {
			return true
		}
	}
	return false
}

// Middleware returns a Gin middleware that enforces a valid Bearer token. When
// a token is rejected, onReject is invoked (e.g. to bump a metric) before the
// 401 is written.
func (v *Verifier) Middleware(onReject func()) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := bearerToken(c.GetHeader("Authorization"))
		if err != nil {
			reject(c, onReject, "missing or malformed Authorization header")
			return
		}
		roles, err := v.verify(c.Request.Context(), raw)
		if err != nil {
			reject(c, onReject, err.Error())
			return
		}
		c.Set(rolesKey, roles)
		c.Next()
	}
}

// RequireRoles returns a middleware that answers 403 unless the token verified
// by Middleware carries at least one of roles. It must run after Middleware;
// without verified roles on the context every request is refused.
func (v *Verifier) RequireRoles(onReject func(), roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenRoles, _ := c.Get(rolesKey)
		have, _ := tokenRoles.([]string)
		if len(roles) == 0 || !hasRequiredRole(have, roles) {
			if onReject != nil {
				onReject()
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":  "forbidden",
				"reason": fmt.Sprintf("missing required role (need one of %v)", roles),
			})
			return
		}
		c.Next()
	}
}

func DisabledMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}

func reject(c *gin.Context, onReject func(), reason string) {
	if onReject != nil {
		onReject()
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "reason": reason})
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", errors.New("no Authorization header")
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
		return "", errors.New("malformed Authorization header")
	}
	return strings.TrimSpace(parts[1]), nil
}
