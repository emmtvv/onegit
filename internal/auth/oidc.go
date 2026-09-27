package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"onegit/internal/config"
	"onegit/internal/store"
)

type OIDC struct {
	cfg      *config.Config
	store    *store.Store
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
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
	// Groups often live only in userinfo.
	if _, ok := all[o.cfg.OIDC.GroupsClaim]; !ok {
		if ui, err := o.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil {
			_ = ui.Claims(&all) // without them the groups claim is simply absent
		}
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
