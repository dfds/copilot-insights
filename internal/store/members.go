package store

import (
	"context"
	"fmt"
)

const (
	EmailVerified = "verified"
	EmailProfile  = "profile"
)

type OrgMember struct {
	UserID int64
	Login  string
	Name   string
	Emails []Email
}

type Email struct {
	Address    string
	Normalized string
	Source     string // EmailVerified or EmailProfile
}

// ReplaceOrgMembers replaces the membership and email rows of each org in
// members, in one transaction. Only orgs present in the map are touched, so a
// caller passes just the orgs whose enumeration completed and the others keep
// their rows. Users left in no org are deleted. primary_email is recomputed
// from the stored emails: verified before profile, then by org and address.
func (s *Store) ReplaceOrgMembers(ctx context.Context, members map[string][]OrgMember) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	upsertUser, err := tx.PrepareContext(ctx,
		`INSERT INTO github_users (user_id, login, name, orgs) VALUES ($1, $2, NULLIF($3, ''), ARRAY[$4::text])
		 ON CONFLICT (user_id) DO UPDATE SET
		   login = EXCLUDED.login,
		   name = EXCLUDED.name,
		   orgs = CASE WHEN $4 = ANY(github_users.orgs) THEN github_users.orgs
		               ELSE ARRAY(SELECT o FROM unnest(array_append(github_users.orgs, $4::text)) o ORDER BY o) END,
		   updated_at = now()`)
	if err != nil {
		return 0, err
	}
	insertEmail, err := tx.PrepareContext(ctx,
		`INSERT INTO github_user_emails (org, user_id, email_normalized, source, email) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, err
	}

	var touched []int64
	written := 0
	for org, list := range members {
		ids := make([]int64, 0, len(list))
		for _, m := range list {
			if _, err := upsertUser.ExecContext(ctx, m.UserID, m.Login, m.Name, org); err != nil {
				return 0, fmt.Errorf("upsert user %d: %w", m.UserID, err)
			}
			ids = append(ids, m.UserID)
		}
		rows, err := tx.QueryContext(ctx,
			`UPDATE github_users SET orgs = array_remove(orgs, $1), updated_at = now()
			 WHERE $1 = ANY(orgs) AND user_id <> ALL($2) RETURNING user_id`, org, ids)
		if err != nil {
			return 0, fmt.Errorf("remove departed members of %s: %w", org, err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			touched = append(touched, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM github_user_emails WHERE org = $1`, org); err != nil {
			return 0, fmt.Errorf("clear emails of %s: %w", org, err)
		}
		for _, m := range list {
			for _, e := range m.Emails {
				if _, err := insertEmail.ExecContext(ctx, org, m.UserID, e.Normalized, e.Source, e.Address); err != nil {
					return 0, fmt.Errorf("insert email of user %d: %w", m.UserID, err)
				}
			}
		}
		touched = append(touched, ids...)
		written += len(list)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM github_user_emails e USING github_users u
		 WHERE e.user_id = u.user_id AND u.orgs = '{}'`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM github_users WHERE orgs = '{}'`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE github_users u SET primary_email = (
		   SELECT e.email FROM github_user_emails e WHERE e.user_id = u.user_id
		   ORDER BY e.source = 'verified' DESC, e.org, e.email_normalized LIMIT 1)
		 WHERE u.user_id = ANY($1)`, touched); err != nil {
		return 0, fmt.Errorf("recompute primary emails: %w", err)
	}
	return written, tx.Commit()
}
