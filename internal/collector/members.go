package collector

import (
	"context"
	"fmt"
	"strings"

	gh "go.dfds.cloud/copilot-insights/internal/github"
	"go.dfds.cloud/copilot-insights/internal/identity"
	"go.dfds.cloud/copilot-insights/internal/store"
	"go.uber.org/zap"
)

// Members refreshes the App installations into scopes, then replaces the
// member and email rows of every org whose enumeration completed. An org that
// errors keeps its existing rows.
func (c *Collectors) Members(ctx context.Context) (Outcome, error) {
	var out Outcome
	installs, err := c.gh.RefreshInstallations(ctx)
	if err != nil {
		return out, fmt.Errorf("list installations: %w", err)
	}
	c.installLoaded.Store(true)

	var scopes []store.ScopeState
	var orgs []string
	haveEnterprise := false
	for _, in := range installs {
		switch in.Scope.Kind {
		case gh.ScopeOrg:
			if c.excluded(in.Scope.Name) {
				continue
			}
			orgs = append(orgs, in.Scope.Name)
		case gh.ScopeEnterprise:
			if !strings.EqualFold(in.Scope.Name, c.cfg.Enterprise) {
				c.logger.Info("ignoring installation on another enterprise", zap.String("enterprise", in.Scope.Name))
				continue
			}
			haveEnterprise = true
		}
		scopes = append(scopes, store.ScopeState{Scope: in.Scope, InstallationID: in.ID})
	}
	if !haveEnterprise {
		// The PAT still reaches enterprise seats without an installation.
		scopes = append(scopes, store.ScopeState{Scope: gh.Enterprise(c.cfg.Enterprise)})
	}
	if err := c.st.SyncScopes(ctx, scopes); err != nil {
		return out, fmt.Errorf("save scopes: %w", err)
	}

	out.Scopes = len(orgs)
	listed := make([][]gh.Member, len(orgs))
	errs := make([]error, len(orgs))
	forEach(len(orgs), c.cfg.Concurrency, func(i int) {
		listed[i], errs[i] = c.gh.OrgMembers(ctx, orgs[i])
	})

	complete := map[string][]store.OrgMember{}
	for i, org := range orgs {
		if errs[i] != nil {
			out.fail(gh.Org(org).String(), errs[i])
			continue
		}
		complete[org] = toOrgMembers(listed[i])
	}
	if len(complete) == 0 {
		return out, nil
	}
	n, err := c.st.ReplaceOrgMembers(ctx, complete)
	if err != nil {
		return out, fmt.Errorf("save members: %w", err)
	}
	out.Items = n
	return out, nil
}

// toOrgMembers keeps every verified-domain email, plus the public profile
// email when it is on a DFDS domain and not already verified.
func toOrgMembers(members []gh.Member) []store.OrgMember {
	out := make([]store.OrgMember, 0, len(members))
	for _, m := range members {
		om := store.OrgMember{UserID: m.UserID, Login: m.Login, Name: m.Name}
		seen := map[string]bool{}
		for _, e := range m.VerifiedEmails {
			n := identity.NormalizeEmail(e)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			om.Emails = append(om.Emails, store.Email{Address: strings.TrimSpace(e), Normalized: n, Source: store.EmailVerified})
		}
		if n := identity.NormalizeEmail(m.Email); n != "" && !seen[n] && identity.IsDfdsTenantEmail(n) {
			om.Emails = append(om.Emails, store.Email{Address: strings.TrimSpace(m.Email), Normalized: n, Source: store.EmailProfile})
		}
		out = append(out, om)
	}
	return out
}
