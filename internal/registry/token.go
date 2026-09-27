package registry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"onegit/internal/auth"
	"onegit/internal/store"
)

// tokenLifetime bounds how long a revoked credential keeps working for pulls
// and pushes already in progress. Clients fetch a new token on 401.
const tokenLifetime = 15 * time.Minute

// Registry tokens are "ogr.<subject>.<unix expiry>.<hmac>". The subject is
// "u<user id>" ("u0": anonymous, public read) or "j<job id>" for a CI job, so
// a job never gets the rights of the user who triggered it. Tokens carry no
// scopes: access is decided per request from the current role or job state.
const tokenPrefix = "ogr."

type tokenSubject struct {
	userID int64
	jobID  int64
}

func subjectOf(u *store.User) string {
	switch {
	case u == nil:
		return "u0"
	case u.Job != nil:
		return "j" + strconv.FormatInt(u.Job.JobID, 10)
	}
	return "u" + strconv.FormatInt(u.ID, 10)
}

func (s *Service) sign(payload string) string {
	m := hmac.New(sha256.New, s.tokenKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) issueToken(subject string, now time.Time) string {
	payload := subject + "." + strconv.FormatInt(now.Add(tokenLifetime).Unix(), 10)
	return tokenPrefix + payload + "." + s.sign(payload)
}

func (s *Service) verifyToken(tok string) (tokenSubject, bool) {
	rest, ok := strings.CutPrefix(tok, tokenPrefix)
	if !ok {
		return tokenSubject{}, false
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 {
		return tokenSubject{}, false
	}
	payload, sig := rest[:i], rest[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return tokenSubject{}, false
	}
	subj, expStr, ok := strings.Cut(payload, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil || time.Now().Unix() > exp || len(subj) < 2 {
		return tokenSubject{}, false
	}
	id, err := strconv.ParseInt(subj[1:], 10, 64)
	if err != nil {
		return tokenSubject{}, false
	}
	switch subj[0] {
	case 'u':
		return tokenSubject{userID: id}, true
	case 'j':
		return tokenSubject{jobID: id}, true
	}
	return tokenSubject{}, false
}

// token implements the docker token endpoint: credentials in Basic auth (a
// password or an access token) are exchanged for a registry token. Without
// credentials an anonymous token is issued when public read is enabled.
func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "use GET with basic auth", nil)
		return
	}
	var who *store.User
	if user, pass, ok := r.BasicAuth(); ok {
		u, err := s.Auth.CheckBasic(r.Context(), user, pass, auth.RequestIP(r, s.Cfg.HTTP.TrustProxy))
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="onegit"`)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error(), nil)
			return
		}
		who = u
	} else if !s.Cfg.Repo.PublicRead {
		w.Header().Set("WWW-Authenticate", `Basic realm="onegit"`)
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", nil)
		return
	}
	now := time.Now()
	tok := s.issueToken(subjectOf(who), now)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"token":        tok,
		"access_token": tok,
		"expires_in":   int(tokenLifetime.Seconds()),
		"issued_at":    now.UTC().Format(time.RFC3339),
	})
}
