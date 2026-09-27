package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Team struct {
	ID          int64
	Name        string
	Description string
	OIDCGroup   string
	CreatedAt   time.Time
	Members     []string // usernames, filled by ListTeams
}

func (s *Store) ListTeams(ctx context.Context) ([]*Team, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, t.name, t.description, t.oidc_group, t.created_at,
		       COALESCE(array_agg(u.username ORDER BY lower(u.username)) FILTER (WHERE u.id IS NOT NULL), '{}')
		FROM teams t LEFT JOIN team_members m ON m.team_id = t.id LEFT JOIN users u ON u.id = m.user_id
		GROUP BY t.id ORDER BY lower(t.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.OIDCGroup, &t.CreatedAt, &t.Members); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) TeamByID(ctx context.Context, id int64) (*Team, error) {
	var t Team
	err := s.db.QueryRow(ctx, `SELECT id, name, description, oidc_group, created_at FROM teams WHERE id = $1`, id).
		Scan(&t.ID, &t.Name, &t.Description, &t.OIDCGroup, &t.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	rows, _ := s.db.Query(ctx, `SELECT u.username FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1 ORDER BY lower(u.username)`, id)
	t.Members, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return &t, err
}

// SaveTeam creates (ID == 0) or updates a team.
func (s *Store) SaveTeam(ctx context.Context, t *Team) error {
	var err error
	if t.ID == 0 {
		err = s.db.QueryRow(ctx, `INSERT INTO teams (name, description, oidc_group) VALUES ($1, $2, $3) RETURNING id, created_at`,
			t.Name, t.Description, t.OIDCGroup).Scan(&t.ID, &t.CreatedAt)
	} else {
		_, err = s.db.Exec(ctx, `UPDATE teams SET name = $2, description = $3, oidc_group = $4 WHERE id = $1`,
			t.ID, t.Name, t.Description, t.OIDCGroup)
	}
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) DeleteTeam(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM teams WHERE id = $1`, id)
	return err
}

func (s *Store) AddTeamMember(ctx context.Context, teamID, userID int64) error {
	_, err := s.db.Exec(ctx, `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, teamID, userID)
	return err
}

func (s *Store) RemoveTeamMember(ctx context.Context, teamID, userID int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID)
	return err
}

// UserTeams returns the lower-cased names of the user's teams.
func (s *Store) UserTeams(ctx context.Context, userID int64) ([]string, error) {
	rows, _ := s.db.Query(ctx, `SELECT lower(t.name) FROM team_members m JOIN teams t ON t.id = m.team_id WHERE m.user_id = $1`, userID)
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TeamMemberIDs maps lower-cased team names to member user ids.
func (s *Store) TeamMemberIDs(ctx context.Context, names []string) (map[string][]int64, error) {
	for i := range names {
		names[i] = strings.ToLower(names[i])
	}
	rows, err := s.db.Query(ctx, `SELECT lower(t.name), m.user_id FROM teams t JOIN team_members m ON m.team_id = t.id
		JOIN users u ON u.id = m.user_id WHERE lower(t.name) = ANY($1) AND u.active`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var n string
		var id int64
		if err := rows.Scan(&n, &id); err != nil {
			return nil, err
		}
		out[n] = append(out[n], id)
	}
	return out, rows.Err()
}

// SyncOIDCTeams sets the user's membership of every team bound to an OIDC
// group from the groups in their latest login.
func (s *Store) SyncOIDCTeams(ctx context.Context, userID int64, groups []string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM team_members m USING teams t
			WHERE m.team_id = t.id AND m.user_id = $1 AND t.oidc_group <> '' AND NOT (t.oidc_group = ANY($2))`, userID, groups); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id)
			SELECT id, $1 FROM teams WHERE oidc_group <> '' AND oidc_group = ANY($2) ON CONFLICT DO NOTHING`, userID, groups)
		return err
	})
}

// ---- audit log ----

type AuditEntry struct {
	ID        int64
	At        time.Time
	ActorName string
	Action    string
	Subject   string
	Data      map[string]any
}

func (s *Store) Audit(ctx context.Context, actorID *int64, action, subject string, data any) error {
	b, err := json.Marshal(data)
	if err != nil || data == nil {
		b = []byte("{}")
	}
	_, err = s.db.Exec(ctx, `INSERT INTO audit_log (actor_id, action, subject, data) VALUES ($1, $2, $3, $4)`,
		actorID, action, subject, b)
	return err
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(ctx, `SELECT a.id, a.at, COALESCE(u.username, 'system'), a.action, a.subject, a.data
		FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id ORDER BY a.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.ActorName, &e.Action, &e.Subject, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
