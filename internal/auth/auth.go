// Package auth handles passwords, API tokens, sessions and OIDC login.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"onegit/internal/kv"
	"onegit/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrTooManyAttempts    = errors.New("too many failed login attempts, try again later")
	// ErrPasswordChangeRequired blocks password-based git/API access until
	// the user has replaced their initial password in the web UI.
	ErrPasswordChangeRequired = errors.New("password change required: sign in to the web UI and set a new password")
)

const (
	TokenPrefix     = "og_"
	JobTokenPrefix  = "ogj_"
	SessionCookie   = "onegit_session"
	SessionLifetime = 30 * 24 * time.Hour

	MinPasswordLength = 10

	loginAttemptLimit  = 10
	loginAttemptWindow = 15 * time.Minute
)

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.DefaultCost)

type Service struct {
	Store *store.Store
	KV    *kv.KV
}

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// ValidatePassword enforces the password policy.
func ValidatePassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return errors.New("password must be at least " + strconv.Itoa(MinPasswordLength) + " characters")
	}
	if len(pw) > 72 { // bcrypt limit
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

func RandomString(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how tokens (access, runner, job) are stored.
func HashToken(s string) string { return hashSecret(s) }

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// CheckPassword verifies a local password login. Failed attempts are
// rate-limited per username and client IP across all replicas (keying on the
// username alone would let anyone lock the admin out).
func (s *Service) CheckPassword(ctx context.Context, username, password, clientIP string) (*store.User, error) {
	rlKey := "login:" + strings.ToLower(username) + ":" + clientIP
	if ok, err := s.KV.Allowed(ctx, rlKey, loginAttemptLimit); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrTooManyAttempts
	}
	u, err := s.Store.UserByUsername(ctx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if u == nil {
		// Burn comparable time to make user enumeration harder.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	}
	if u == nil || !u.Active || u.PasswordHash == "" ||
		bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		if _, err := s.KV.Hit(ctx, rlKey, loginAttemptLimit, loginAttemptWindow); err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}
	_ = s.KV.ResetHits(ctx, rlKey) // a stale counter only delays a future lockout
	return u, nil
}

// CheckBasic authenticates git-over-HTTP / API basic auth. The password may be
// either the user's password or a personal access token (the only option for
// SSO-only users).
func (s *Service) CheckBasic(ctx context.Context, username, secret, clientIP string) (*store.User, error) {
	if strings.HasPrefix(secret, TokenPrefix) || strings.HasPrefix(secret, JobTokenPrefix) {
		// The username is ignored: git clients require one, but the token
		// alone identifies the user.
		return s.CheckToken(ctx, secret)
	}
	u, err := s.CheckPassword(ctx, username, secret, clientIP)
	if err != nil {
		return nil, err
	}
	if u.MustChangePassword {
		return nil, ErrPasswordChangeRequired
	}
	return u, nil
}

func (s *Service) CheckToken(ctx context.Context, token string) (*store.User, error) {
	if strings.HasPrefix(token, JobTokenPrefix) {
		return s.checkJobToken(ctx, token)
	}
	u, err := s.Store.UserByTokenHash(ctx, hashSecret(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if !u.Active {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// checkJobToken turns a running job's token into a synthetic read-only user.
func (s *Service) checkJobToken(ctx context.Context, token string) (*store.User, error) {
	j, err := s.Store.RunningJobByTokenHash(ctx, hashSecret(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return s.jobUser(ctx, j)
}

// JobUser returns the synthetic user of a job, which must still be running
// (registry tokens issued to jobs carry the job id, not a user).
func (s *Service) JobUser(ctx context.Context, jobID int64) (*store.User, error) {
	j, err := s.Store.JobByID(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && j.Status != store.JobRunning) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return s.jobUser(ctx, j)
}

func (s *Service) jobUser(ctx context.Context, j *store.Job) (*store.User, error) {
	run, err := s.Store.RunByID(ctx, j.RunID)
	if err != nil {
		return nil, err
	}
	u := &store.User{
		Username: "ci-job-" + strconv.FormatInt(j.ID, 10),
		Role:     store.RoleRead,
		Active:   true,
		Job: &store.JobIdentity{JobID: j.ID, RunID: run.ID, Kind: j.Kind, SHA: run.SHA, Ref: run.Ref,
			UserID: run.TriggeredBy},
	}
	if run.TriggeredBy != nil {
		u.ID = *run.TriggeredBy
	}
	return u, nil
}

// NewToken creates a personal access token and returns its plaintext once.
func (s *Service) NewToken(ctx context.Context, userID int64, name string) (string, *store.Token, error) {
	plain := TokenPrefix + RandomString(30)
	t := &store.Token{UserID: userID, Name: name, Prefix: plain[:len(TokenPrefix)+6]}
	if err := s.Store.AddToken(ctx, t, hashSecret(plain)); err != nil {
		return "", nil, err
	}
	return plain, t, nil
}

func sessionKey(id string) string { return "session:" + hashSecret(id) }

func (s *Service) NewSession(ctx context.Context, userID int64) (string, error) {
	id := RandomString(32)
	if err := s.KV.Set(ctx, sessionKey(id), strconv.FormatInt(userID, 10), SessionLifetime); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) SessionUser(ctx context.Context, id string) (*store.User, error) {
	v, ok, err := s.KV.Get(ctx, sessionKey(id))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	uid, _ := strconv.ParseInt(v, 10, 64)
	u, err := s.Store.UserByID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if !u.Active {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

func (s *Service) EndSession(ctx context.Context, id string) error {
	return s.KV.Del(ctx, sessionKey(id))
}

// SetPassword validates and stores a new password and clears the
// must-change flag.
func (s *Service) SetPassword(ctx context.Context, u *store.User, pw string) error {
	if err := ValidatePassword(pw); err != nil {
		return err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	u.PasswordHash, u.MustChangePassword = hash, false
	return s.Store.UpdateUser(ctx, u)
}

// RequestIP returns the client IP. With trustProxy it honours X-Real-IP or
// the last X-Forwarded-For hop (the one appended by our own proxy).
func RequestIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
