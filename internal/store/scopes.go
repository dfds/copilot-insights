package store

import (
	"context"
	"database/sql"
	"fmt"

	"go.dfds.cloud/copilot-insights/internal/github"
)

// ScopeState is one row of the scopes table. InstallationID is 0 when the App
// is not installed on the scope (the enterprise when only the PAT reaches it).
type ScopeState struct {
	Scope          github.Scope
	InstallationID int64
	Active         bool
	CopilotEnabled bool
}

// SyncScopes upserts scopes as active and marks every other scope inactive.
// Rows are never deleted, so data keyed on a vanished scope stays joinable.
func (s *Store) SyncScopes(ctx context.Context, scopes []ScopeState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	keys := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO scopes (kind, name, installation_id) VALUES ($1, $2, NULLIF($3, 0))
			 ON CONFLICT (kind, name) DO UPDATE
			 SET installation_id = EXCLUDED.installation_id, active = true, last_seen_at = now()`,
			string(sc.Scope.Kind), sc.Scope.Name, sc.InstallationID)
		if err != nil {
			return fmt.Errorf("upsert scope %s: %w", sc.Scope, err)
		}
		keys = append(keys, sc.Scope.String())
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE scopes SET active = false WHERE active AND kind || ':' || name <> ALL($1)`, keys); err != nil {
		return fmt.Errorf("deactivate scopes: %w", err)
	}
	return tx.Commit()
}

// ActiveScopes returns the active scopes ordered by kind and name.
func (s *Store) ActiveScopes(ctx context.Context) ([]ScopeState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, name, installation_id, active, copilot_enabled FROM scopes
		 WHERE active ORDER BY kind, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeState
	for rows.Next() {
		var st ScopeState
		var kind string
		var inst sql.NullInt64
		if err := rows.Scan(&kind, &st.Scope.Name, &inst, &st.Active, &st.CopilotEnabled); err != nil {
			return nil, err
		}
		st.Scope.Kind = github.ScopeKind(kind)
		st.InstallationID = inst.Int64
		out = append(out, st)
	}
	return out, rows.Err()
}

// SetCopilotEnabled records whether a scope's Copilot endpoints answer.
func (s *Store) SetCopilotEnabled(ctx context.Context, scope github.Scope, enabled bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE scopes SET copilot_enabled = $3 WHERE kind = $1 AND name = $2`,
		string(scope.Kind), scope.Name, enabled)
	return err
}
