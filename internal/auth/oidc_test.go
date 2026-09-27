package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"onegit/internal/auth"
	"onegit/internal/config"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

// fakeIdP is a minimal OpenID provider: discovery, JWKS, token (with PKCE
// check) and userinfo endpoints, issuing RS256 id_tokens.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	codes    map[string]map[string]any // code → id_token claims
	userinfo map[string]any
	// challenge is the PKCE challenge of the last authorization request.
	challenge string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, codes: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "userinfo_endpoint": f.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		claims, ok := f.codes[r.Form.Get("code")]
		challenge := f.challenge
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + r.Form.Get("code"), "token_type": "Bearer", "expires_in": 60,
			"id_token": f.sign(claims),
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.userinfo)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	full := map[string]any{"iss": f.srv.URL, "aud": "onegit", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		full[k] = v
	}
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	payload, _ := json.Marshal(full)
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

// login runs the code flow for claims and returns the resulting user.
func (f *fakeIdP) login(t *testing.T, o *auth.OIDC, claims map[string]any) (*store.User, error) {
	t.Helper()
	nonce, verifier := auth.RandomString(8), auth.RandomString(32)
	u, err := url.Parse(o.AuthURL("state-1", nonce, verifier))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("state") != "state-1" || q.Get("nonce") != nonce || q.Get("code_challenge_method") != "S256" ||
		q.Get("client_id") != "onegit" || !strings.HasSuffix(q.Get("redirect_uri"), "/login/oidc/callback") {
		t.Fatalf("auth URL = %s", u)
	}
	code := auth.RandomString(8)
	if _, ok := claims["nonce"]; !ok {
		claims["nonce"] = nonce
	}
	f.mu.Lock()
	f.codes[code] = claims
	f.challenge = q.Get("code_challenge")
	f.mu.Unlock()
	return o.Exchange(ctx, code, nonce, verifier)
}

func oidcSetup(t *testing.T) (*fakeIdP, *config.Config, *store.Store) {
	f := newFakeIdP(t)
	cfg := testutil.Config(t)
	cfg.OIDC.Enabled, cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = true, f.srv.URL, "onegit", "secret"
	cfg.OIDC.AdminGroup, cfg.OIDC.WriteGroup = "admins", "devs"
	return f, cfg, testutil.Store(t, cfg)
}

func TestOIDCLogin(t *testing.T) {
	t.Parallel()
	f, cfg, st := oidcSetup(t)
	o, err := auth.NewOIDC(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	team := &store.Team{Name: "platform", OIDCGroup: "platform-group"}
	testutil.Must(t, st.SaveTeam(ctx, team))

	// First login registers the user with a role from the groups claim.
	u, err := f.login(t, o, map[string]any{"sub": "sub-1", "email": "Zoe.Q@example.com", "name": "Zoe Q",
		"preferred_username": "zoe q!", "groups": []any{"devs", "platform-group"}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "zoe-q" || u.Role != store.RoleWrite || !u.HasSSO() || *u.OIDCSubject != "sub-1" || u.FullName != "Zoe Q" {
		t.Errorf("registered user = %+v", u)
	}
	teams, _ := st.UserTeams(ctx, u.ID)
	if len(teams) != 1 || teams[0] != "platform" {
		t.Errorf("teams = %v", teams)
	}

	// Next login re-syncs the role and teams (groups changed at the IdP).
	u2, err := f.login(t, o, map[string]any{"sub": "sub-1", "email": "zoe@example.com", "groups": []any{"admins"}})
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID != u.ID || u2.Role != store.RoleAdmin || u2.Email != "zoe@example.com" || u2.FullName != "Zoe Q" {
		t.Errorf("re-login = %+v", u2)
	}
	if teams, _ := st.UserTeams(ctx, u.ID); len(teams) != 0 {
		t.Errorf("team membership not removed: %v", teams)
	}
	u3, _ := f.login(t, o, map[string]any{"sub": "sub-1", "groups": "nobody"})
	if u3.Role != store.RoleRead {
		t.Errorf("user outside mapped groups gets role %q, want the default", u3.Role)
	}

	// Username collisions get a number; groups can come from userinfo.
	testutil.User(t, st, "sam", store.RoleRead)
	f.mu.Lock()
	f.userinfo = map[string]any{"sub": "sub-2", "groups": []any{"devs"}}
	f.mu.Unlock()
	s2, err := f.login(t, o, map[string]any{"sub": "sub-2", "email": "sam@corp.example"})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Username != "sam2" || s2.Role != store.RoleWrite {
		t.Errorf("second sam = %+v", s2)
	}

	// Disabled accounts and wrong nonces are refused.
	s2.Active = false
	testutil.Must(t, st.UpdateUser(ctx, s2))
	if _, err := f.login(t, o, map[string]any{"sub": "sub-2"}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled account: %v", err)
	}
	if _, err := f.login(t, o, map[string]any{"sub": "sub-3", "nonce": "other"}); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Errorf("nonce mismatch: %v", err)
	}
	if _, err := o.Exchange(ctx, "unknown-code", "n", "v"); err == nil {
		t.Error("unknown code accepted")
	}
}

func TestOIDCWithoutAutoRegister(t *testing.T) {
	t.Parallel()
	f, cfg, st := oidcSetup(t)
	cfg.OIDC.AutoRegister = false
	cfg.OIDC.AdminGroup, cfg.OIDC.WriteGroup = "", ""
	o, err := auth.NewOIDC(ctx, cfg, st)
	testutil.Must(t, err)
	if _, err := f.login(t, o, map[string]any{"sub": "new"}); err == nil || !strings.Contains(err.Error(), "no local account") {
		t.Errorf("unknown identity without auto-register: %v", err)
	}
	// A linked account logs in, and without group mapping its role is kept.
	u := testutil.User(t, st, "linked", store.RoleWrite)
	sub := "linked-sub"
	u.OIDCSubject = &sub
	testutil.Must(t, st.UpdateUser(ctx, u))
	got, err := f.login(t, o, map[string]any{"sub": sub, "groups": []any{}})
	if err != nil || got.ID != u.ID || got.Role != store.RoleWrite {
		t.Errorf("linked login = %+v, %v", got, err)
	}
}

func TestOIDCDiscoveryFailure(t *testing.T) {
	cfg := config.Default()
	cfg.OIDC.Issuer = "http://127.0.0.1:1"
	if _, err := auth.NewOIDC(ctx, cfg, nil); err == nil {
		t.Error("unreachable issuer accepted")
	}
}
