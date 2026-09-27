package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

const seatsPageSize = 100

// Seats lists every Copilot seat on an org or the enterprise. Enterprise
// seats need the PAT and return ErrNoPAT without one.
func (c *Client) Seats(ctx context.Context, scope Scope) ([]Seat, error) {
	var out []Seat
	for page := 1; ; page++ {
		path := fmt.Sprintf("%s/copilot/billing/seats?per_page=%d&page=%d", scopeBase(scope), seatsPageSize, page)
		var p seatsPage
		if err := c.getJSON(ctx, scope, familySeats, path, &p); err != nil {
			return nil, err
		}
		for _, s := range p.Seats {
			if s.Assignee == nil || s.Assignee.ID == 0 {
				continue
			}
			seat := Seat{
				UserID:                  s.Assignee.ID,
				Login:                   s.Assignee.Login,
				PlanType:                s.PlanType,
				CreatedAt:               s.CreatedAt,
				UpdatedAt:               s.UpdatedAt,
				LastActivityAt:          s.LastActivityAt,
				LastAuthenticatedAt:     s.LastAuthenticatedAt,
				LastActivityEditor:      deref(s.LastActivityEditor),
				PendingCancellationDate: deref(s.PendingCancellationDate),
			}
			if s.AssigningTeam != nil {
				seat.AssigningTeamSlug = s.AssigningTeam.Slug
			}
			switch {
			case s.Organization != nil:
				seat.OrgName = s.Organization.Login
			case scope.Kind == ScopeOrg:
				seat.OrgName = scope.Name
			}
			out = append(out, seat)
		}
		if len(p.Seats) < seatsPageSize {
			return out, nil
		}
	}
}

// CopilotBilling fetches an org's seat breakdown and Copilot policy settings.
func (c *Client) CopilotBilling(ctx context.Context, org string) (*CopilotBilling, error) {
	path := "/orgs/" + url.PathEscape(org) + "/copilot/billing"
	var raw map[string]json.RawMessage
	if err := c.getJSON(ctx, Org(org), familyCopilotBilling, path, &raw); err != nil {
		return nil, err
	}
	out := &CopilotBilling{Policies: map[string]json.RawMessage{}}
	for k, v := range raw {
		var err error
		switch k {
		case "seat_breakdown":
			err = json.Unmarshal(v, &out.SeatBreakdown)
		case "seat_management_setting":
			err = json.Unmarshal(v, &out.SeatManagementSetting)
		case "plan_type":
			err = json.Unmarshal(v, &out.PlanType)
		default:
			out.Policies[k] = v
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s field %s: %w", path, k, err)
		}
	}
	return out, nil
}
