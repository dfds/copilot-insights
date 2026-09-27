package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// organizationVerifiedDomainEmails returns every verified-domain email of the
// member, not only the first; the identity mapping keeps all of them.
const membersQuery = `query($org: String!, $cursor: String) {
  organization(login: $org) {
    membersWithRole(first: 100, after: $cursor) {
      pageInfo { hasNextPage endCursor }
      nodes { databaseId login name email organizationVerifiedDomainEmails(login: $org) }
    }
  }
}`

type graphQLErrors []struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e graphQLErrors) err() error {
	if len(e) == 0 {
		return nil
	}
	if len(e) == 1 {
		return fmt.Errorf("graphql: %s", e[0].Message)
	}
	return fmt.Errorf("graphql: %s (and %d more errors)", e[0].Message, len(e)-1)
}

// OrgMembers lists every member of an org. Any GraphQL error fails the whole
// call, so a caller never mistakes a partial listing for the full one.
func (c *Client) OrgMembers(ctx context.Context, org string) ([]Member, error) {
	var out []Member
	var cursor *string
	for {
		body, err := json.Marshal(map[string]any{
			"query":     membersQuery,
			"variables": map[string]any{"org": org, "cursor": cursor},
		})
		if err != nil {
			return nil, err
		}
		r, err := c.call(ctx, Org(org), familyMembers, http.MethodPost, "/graphql", body)
		if err != nil {
			return nil, err
		}
		var gr struct {
			Data struct {
				Organization *struct {
					MembersWithRole struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							DatabaseID     int64    `json:"databaseId"`
							Login          string   `json:"login"`
							Name           *string  `json:"name"`
							Email          *string  `json:"email"`
							VerifiedEmails []string `json:"organizationVerifiedDomainEmails"`
						} `json:"nodes"`
					} `json:"membersWithRole"`
				} `json:"organization"`
			} `json:"data"`
			Errors graphQLErrors `json:"errors"`
		}
		if err := json.Unmarshal(r.body, &gr); err != nil {
			return nil, fmt.Errorf("decode members of %s: %w", org, err)
		}
		if err := gr.Errors.err(); err != nil {
			return nil, fmt.Errorf("members of %s: %w", org, err)
		}
		if gr.Data.Organization == nil {
			return nil, fmt.Errorf("members of %s: organization not found", org)
		}
		m := gr.Data.Organization.MembersWithRole
		for _, n := range m.Nodes {
			if n.Login == "" || strings.HasSuffix(n.Login, "[bot]") {
				continue
			}
			if n.DatabaseID == 0 {
				c.logger.Warn("org member without databaseId skipped", zap.String("org", org), zap.String("login", n.Login))
				continue
			}
			out = append(out, Member{
				UserID:         n.DatabaseID,
				Login:          n.Login,
				Name:           deref(n.Name),
				Email:          deref(n.Email),
				VerifiedEmails: n.VerifiedEmails,
			})
		}
		if !m.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = &m.PageInfo.EndCursor
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
