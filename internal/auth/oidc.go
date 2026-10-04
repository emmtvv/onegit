package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"onegit/internal/avatars"
	"onegit/internal/config"
	"onegit/internal/store"
)

type OIDC struct {
	cfg      *config.Config
	store    *store.Store
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config

	// Avatars, when set, receives the IdP's picture claim.
	Avatars *avatars.Service
}

func NewOIDC(ctx context.Context, cfg *config.Config, st *store.Store) (*OIDC, error) {
	p, err := oidc.NewProvider(ctx, cfg.OIDC.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	return &OIDC{
		cfg:      cfg,
		store:    st,
		provider: p,
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.OIDC.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  cfg.HTTP.BaseURL + "/login/oidc/callback",
			Scopes:       cfg.OIDC.Scopes,
		},
	}, nil
}

// AuthURL returns the IdP redirect URL. state and nonce must be stored by the
// caller (cookie) and passed back to Exchange.
func (o *OIDC) AuthURL(state, nonce, verifier string) string {
	return o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

type oidcClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
	Nonce             string `json:"nonce"`
}

// Exchange completes the code flow and returns the local user, creating or
// updating it as configured.
func (o *OIDC) Exchange(ctx context.Context, code, nonce, verifier string) (*store.User, error) {
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in response")
	}
	idt, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}
	var c oidcClaims
	all := map[string]any{}
	if err := idt.Claims(&c); err != nil {
		return nil, err
	}
	if err := idt.Claims(&all); err != nil {
		return nil, err
	}
	if c.Nonce != nonce {
		return nil, errors.New("nonce mismatch")
	}
	// Groups and the picture often live only in userinfo ("thin" ID tokens).
	_, hasGroups := all[o.cfg.OIDC.GroupsClaim]
	if !hasGroups || (c.Picture == "" && o.Avatars != nil) {
		// Userinfo claims count only for the same subject (OIDC Core 5.3.2);
		// without them the groups claim and picture are simply absent.
		if ui, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == c.Subject {
			_ = ui.Claims(&all)
		}
	}
	if c.Picture == "" {
		c.Picture, _ = all["picture"].(string)
	}
	groups := claimStrings(all[o.cfg.OIDC.GroupsClaim])
	role := o.roleFor(groups)

	u, err := o.store.UserByOIDCSubject(ctx, c.Subject)
	switch {
	case err == nil:
		if !u.Active {
			return nil, errors.New("account is disabled")
		}
		u.Email, u.FullName = firstNonEmpty(c.Email, u.Email), firstNonEmpty(c.Name, u.FullName)
		if role != "" {
			u.Role = role
		}
		if err := o.store.UpdateUser(ctx, u); err != nil {
			return nil, err
		}
		if err := o.syncAvatar(ctx, u, c.Picture); err != nil {
			return nil, err
		}
		return u, o.syncTeams(ctx, u, all)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	if !o.cfg.OIDC.AutoRegister {
		return nil, errors.New("no local account is linked to this identity")
	}
	if role == "" {
		role = store.Role(o.cfg.OIDC.DefaultRole)
	}
	base := sanitizeUsername(firstNonEmpty(c.PreferredUsername, strings.Split(c.Email, "@")[0], "user"))
	u = &store.User{
		Email: c.Email, FullName: c.Name, Role: role, Active: true,
		OIDCSubject: &c.Subject,
	}
	for i := 0; i < 100; i++ {
		u.Username = base
		if i > 0 {
			u.Username = fmt.Sprintf("%s%d", base, i+1)
		}
		if _, err := o.store.UserByUsername(ctx, u.Username); errors.Is(err, store.ErrNotFound) {
			if err := o.store.CreateUser(ctx, u); err != nil {
				return nil, err
			}
			if err := o.syncAvatar(ctx, u, c.Picture); err != nil {
				return nil, err
			}
			return u, o.syncTeams(ctx, u, all)
		}
	}
	return nil, errors.New("could not allocate a username")
}

// syncTeams updates membership of teams bound to OIDC groups, when the IdP
// sent a groups claim at all.
func (o *OIDC) syncTeams(ctx context.Context, u *store.User, claims map[string]any) error {
	raw, ok := claims[o.cfg.OIDC.GroupsClaim]
	if !ok {
		return nil
	}
	groups := claimStrings(raw)
	if groups == nil {
		groups = []string{}
	}
	return o.store.SyncOIDCTeams(ctx, u.ID, groups)
}

// syncAvatar copies the IdP's picture into the user's avatar, which then
// cannot be changed in onegit. Without a picture claim an earlier IdP
// avatar is dropped and the user may upload their own. A picture that
// cannot be fetched leaves the stored avatar alone and never fails login.
func (o *OIDC) syncAvatar(ctx context.Context, u *store.User, picture string) error {
	if o.Avatars == nil {
		return nil
	}
	if picture == "" {
		return o.Avatars.Delete(ctx, u.ID, store.AvatarOIDC)
	}
	data, err := fetchPicture(ctx, picture)
	if err == nil {
		err = o.Avatars.Set(ctx, u.ID, store.AvatarOIDC, data)
	}
	if err != nil {
		slog.Warn("oidc: store picture", "user", u.Username, "err", err)
	}
	return nil
}

func fetchPicture(ctx context.Context, rawURL string) ([]byte, error) {
	if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("unsupported picture URL %q", rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("picture: %s", resp.Status)
	}
	// One byte over the limit is enough for Set to refuse it.
	return io.ReadAll(io.LimitReader(resp.Body, avatars.MaxSize+1))
}

// roleFor maps IdP groups to a role. Empty result means "don't touch".
func (o *OIDC) roleFor(groups []string) store.Role {
	cfg := o.cfg.OIDC
	if cfg.AdminGroup == "" && cfg.WriteGroup == "" {
		return ""
	}
	switch {
	case cfg.AdminGroup != "" && slices.Contains(groups, cfg.AdminGroup):
		return store.RoleAdmin
	case cfg.WriteGroup != "" && slices.Contains(groups, cfg.WriteGroup):
		return store.RoleWrite
	}
	return store.Role(cfg.DefaultRole)
}

func claimStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{t}
	}
	return nil
}

var usernameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeUsername(s string) string {
	s = strings.Trim(usernameRe.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "user"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
