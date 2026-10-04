package store

import (
	"context"
	"time"
)

type Role string

const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

func (r Role) Valid() bool { return r == RoleRead || r == RoleWrite || r == RoleAdmin }

func (r Role) CanWrite() bool { return r == RoleWrite || r == RoleAdmin }

type User struct {
	ID                 int64
	Username           string
	Email              string
	FullName           string
	PasswordHash       string
	MustChangePassword bool
	Role               Role
	Active             bool
	OIDCSubject        *string
	CreatedAt          time.Time

	// Job is set (and the user is synthetic, never saved) when a CI job
	// authenticated with its job token.
	Job *JobIdentity
}

// JobIdentity describes the CI job behind a job token. It can read the
// repository; pipeline jobs can also push to the registry, which records
// them as the build's provenance.
type JobIdentity struct {
	JobID  int64
	RunID  int64
	Kind   string // "ci" or "deploy"
	SHA    string
	Ref    string
	UserID *int64 // who triggered the run
}

func (u *User) IsAdmin() bool  { return u != nil && u.Active && u.Role == RoleAdmin }
func (u *User) CanWrite() bool { return u != nil && u.Active && u.Role.CanWrite() }
func (u *User) HasSSO() bool   { return u.OIDCSubject != nil }

func (u *User) DisplayName() string {
	if u.FullName != "" {
		return u.FullName
	}
	return u.Username
}

const userCols = `id, username, email, full_name, password_hash, must_change_password, role, active, oidc_subject, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.FullName, &u.PasswordHash, &u.MustChangePassword,
		&u.Role, &u.Active, &u.OIDCSubject, &u.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &u, nil
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	err := s.db.QueryRow(ctx,
		`INSERT INTO users (username, email, full_name, password_hash, must_change_password, role, active, oidc_subject)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, created_at`,
		u.Username, u.Email, u.FullName, u.PasswordHash, u.MustChangePassword, u.Role, u.Active, u.OIDCSubject,
	).Scan(&u.ID, &u.CreatedAt)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

// CreateInitialAdmin creates the bootstrap admin only if there are no users
// at all. It is safe to call concurrently from several replicas; the return
// value reports whether this call created the user.
func (s *Store) CreateInitialAdmin(ctx context.Context, u *User) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`INSERT INTO users (username, email, password_hash, must_change_password, role, active)
		 SELECT $1, $2, $3, true, 'admin', true
		 WHERE NOT EXISTS (SELECT 1 FROM users)
		 ON CONFLICT DO NOTHING`,
		u.Username, u.Email, u.PasswordHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	_, err := s.db.Exec(ctx,
		`UPDATE users SET username=$1, email=$2, full_name=$3, password_hash=$4, must_change_password=$5,
		 role=$6, active=$7, oidc_subject=$8 WHERE id=$9`,
		u.Username, u.Email, u.FullName, u.PasswordHash, u.MustChangePassword, u.Role, u.Active, u.OIDCSubject, u.ID)
	if isUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM users WHERE id=$1`, id)
	return err
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (s *Store) UserByUsername(ctx context.Context, name string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE lower(username)=lower($1)`, name))
}

func (s *Store) UserByOIDCSubject(ctx context.Context, sub string) (*User, error) {
	return scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE oidc_subject=$1`, sub))
}

// FindUsers lists users whose username, name or email contains q, by
// username, and counts all matches.
func (s *Store) FindUsers(ctx context.Context, q string, limit, offset int) ([]*User, int, error) {
	var total int
	cond := `($1 = '' OR strpos(lower(username), lower($1)) > 0 OR strpos(lower(full_name), lower($1)) > 0
		OR strpos(lower(email), lower($1)) > 0)`
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM users WHERE `+cond, q).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users WHERE `+cond+` ORDER BY lower(username) LIMIT $2 OFFSET $3`,
		q, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY lower(username)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
