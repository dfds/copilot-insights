package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.dfds.cloud/copilot-insights/internal/github"
)

// SaveSeats replaces the scope's copilot_seats_current rows and its snapshot
// for day with seats. Callers guard against an empty listing; this method
// treats it as "no seats" and empties both.
func (s *Store) SaveSeats(ctx context.Context, scope github.Scope, day time.Time, seats []github.Seat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	kind, name, date := string(scope.Kind), scope.Name, day.UTC().Format(time.DateOnly)
	ids := make([]int64, 0, len(seats))
	for _, st := range seats {
		ids = append(ids, st.UserID)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM copilot_seats_current WHERE scope_kind = $1 AND scope_name = $2 AND user_id <> ALL($3)`,
		kind, name, ids); err != nil {
		return fmt.Errorf("prune current seats: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM copilot_seats_snapshot
		 WHERE scope_kind = $1 AND scope_name = $2 AND snapshot_date = $3 AND user_id <> ALL($4)`,
		kind, name, date, ids); err != nil {
		return fmt.Errorf("prune seat snapshot: %w", err)
	}

	const cols = `login, org_name, assigning_team_slug, plan_type, created_at, updated_at,
		last_activity_at, last_activity_editor, last_authenticated_at, pending_cancellation_date`
	const set = `login = EXCLUDED.login, org_name = EXCLUDED.org_name,
		assigning_team_slug = EXCLUDED.assigning_team_slug, plan_type = EXCLUDED.plan_type,
		created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at,
		last_activity_at = EXCLUDED.last_activity_at, last_activity_editor = EXCLUDED.last_activity_editor,
		last_authenticated_at = EXCLUDED.last_authenticated_at,
		pending_cancellation_date = EXCLUDED.pending_cancellation_date`
	const vals = `$4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9, $10,
		NULLIF($11, ''), $12, NULLIF($13, '')::date`
	current, err := tx.PrepareContext(ctx,
		`INSERT INTO copilot_seats_current (scope_kind, scope_name, user_id, `+cols+`)
		 VALUES ($1, $2, $3, `+vals+`)
		 ON CONFLICT (scope_kind, scope_name, user_id) DO UPDATE SET `+set)
	if err != nil {
		return err
	}
	snapshot, err := tx.PrepareContext(ctx,
		`INSERT INTO copilot_seats_snapshot (scope_kind, scope_name, user_id, `+cols+`, snapshot_date)
		 VALUES ($1, $2, $3, `+vals+`, $14)
		 ON CONFLICT (scope_kind, scope_name, snapshot_date, user_id) DO UPDATE SET `+set)
	if err != nil {
		return err
	}
	for _, st := range seats {
		args := []any{kind, name, st.UserID, st.Login, st.OrgName, st.AssigningTeamSlug, st.PlanType,
			st.CreatedAt, st.UpdatedAt, st.LastActivityAt, st.LastActivityEditor, st.LastAuthenticatedAt,
			st.PendingCancellationDate}
		if _, err := current.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("upsert current seat %d: %w", st.UserID, err)
		}
		if _, err := snapshot.ExecContext(ctx, append(args, date)...); err != nil {
			return fmt.Errorf("upsert seat snapshot %d: %w", st.UserID, err)
		}
	}
	return tx.Commit()
}

// SaveCopilotBilling upserts an org's billing summary for day.
func (s *Store) SaveCopilotBilling(ctx context.Context, org string, day time.Time, b *github.CopilotBilling) error {
	policies, err := json.Marshal(b.Policies)
	if err != nil {
		return err
	}
	sb := b.SeatBreakdown
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO copilot_billing_summary (org_name, snapshot_date, seat_total, seat_added_this_cycle,
		   seat_pending_invitation, seat_pending_cancellation, seat_active_this_cycle, seat_inactive_this_cycle,
		   plan_type, seat_management_setting, policies)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11)
		 ON CONFLICT (org_name, snapshot_date) DO UPDATE SET
		   seat_total = EXCLUDED.seat_total, seat_added_this_cycle = EXCLUDED.seat_added_this_cycle,
		   seat_pending_invitation = EXCLUDED.seat_pending_invitation,
		   seat_pending_cancellation = EXCLUDED.seat_pending_cancellation,
		   seat_active_this_cycle = EXCLUDED.seat_active_this_cycle,
		   seat_inactive_this_cycle = EXCLUDED.seat_inactive_this_cycle,
		   plan_type = EXCLUDED.plan_type, seat_management_setting = EXCLUDED.seat_management_setting,
		   policies = EXCLUDED.policies, fetched_at = now()`,
		org, day.UTC().Format(time.DateOnly), sb.Total, sb.AddedThisCycle, sb.PendingInvitation,
		sb.PendingCancellation, sb.ActiveThisCycle, sb.InactiveThisCycle, b.PlanType, b.SeatManagementSetting,
		string(policies))
	return err
}
