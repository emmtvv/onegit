package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"onegit/internal/git"
	"onegit/internal/hooks"
)

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.internalToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req hooks.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var resp hooks.Response
	switch r.PathValue("name") {
	case "pre-receive":
		resp = s.preReceive(r.Context(), &req)
	case "post-receive":
		resp = s.postReceive(r.Context(), &req)
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func reject(format string, args ...any) hooks.Response {
	return hooks.Response{Reject: true, Message: "onegit: " + fmt.Sprintf(format, args...)}
}

// preReceive enforces push policy. Branch protection rules plug in here.
func (s *Server) preReceive(ctx context.Context, req *hooks.Request) hooks.Response {
	u, err := s.Store.UserByID(ctx, req.PusherID)
	if err != nil || !u.CanWrite() {
		return reject("you do not have write access to this repository")
	}
	for _, up := range req.Updates {
		switch {
		case strings.HasPrefix(up.Ref, "refs/heads/"), strings.HasPrefix(up.Ref, "refs/tags/"):
		default:
			return reject("pushing to %s is not allowed; only branches and tags can be pushed", up.Ref)
		}
		branch, isBranch := strings.CutPrefix(up.Ref, "refs/heads/")
		if isBranch && up.NewSHA == git.ZeroSHA && branch == s.defaultBranch(ctx) {
			return reject("the default branch %q cannot be deleted", branch)
		}
	}
	msg, err := s.Pulls.CheckPush(ctx, req.Updates, req.GitEnv)
	if err != nil {
		s.Log.Error("branch protection check", "err", err)
		return reject("internal error while checking branch protection")
	}
	if msg != "" {
		return reject("%s", msg)
	}
	return hooks.Response{}
}

// postReceive runs after refs are updated: syncs pull requests, starts
// pipelines and tells the pusher where to open or view a PR.
func (s *Server) postReceive(ctx context.Context, req *hooks.Request) hooks.Response {
	s.ensureHead(ctx, req.Updates)
	pusher, _ := s.Store.UserByID(ctx, req.PusherID)
	s.Pulls.SyncPush(ctx, pusher, req.Updates)
	// Pipelines start in the background: the pusher should not wait.
	go s.CI.OnPush(context.WithoutCancel(ctx), pusher, req.Updates)
	var msg strings.Builder
	for _, up := range req.Updates {
		branch, ok := strings.CutPrefix(up.Ref, "refs/heads/")
		if !ok || up.NewSHA == git.ZeroSHA {
			continue
		}
		s.Log.Info("push", "user", req.PusherName, "ref", up.Ref, "old", short(up.OldSHA), "new", short(up.NewSHA))
		if branch == s.defaultBranch(ctx) {
			continue
		}
		open, _ := s.Store.OpenPullsForBranch(ctx, branch)
		shown := false
		for _, p := range open {
			if p.HeadBranch == branch {
				fmt.Fprintf(&msg, "\nView pull request #%d for %q:\n  %s\n", p.ID, branch, s.Pulls.PullURL(p.ID))
				shown = true
			}
		}
		if !shown {
			fmt.Fprintf(&msg, "\nCreate a pull request for %q:\n  %s\n", branch, s.Pulls.CreateURL(branch))
		}
	}
	return hooks.Response{Message: msg.String()}
}

// ensureHead points HEAD at a real branch after the first push.
func (s *Server) ensureHead(ctx context.Context, updates []hooks.RefUpdate) {
	head := s.Repo.HeadBranch(ctx)
	if head != "" {
		if _, err := s.Repo.ResolveCommit(ctx, "refs/heads/"+head); err == nil {
			return
		}
	}
	var candidate string
	for _, up := range updates {
		b, ok := strings.CutPrefix(up.Ref, "refs/heads/")
		if !ok || up.NewSHA == git.ZeroSHA {
			continue
		}
		if b == s.Cfg.Repo.DefaultBranch {
			candidate = b
			break
		}
		if candidate == "" {
			candidate = b
		}
	}
	if candidate != "" {
		if err := s.Repo.SetHead(ctx, candidate); err != nil {
			s.Log.Warn("set HEAD", "err", err)
		}
	}
}

func (s *Server) defaultBranch(ctx context.Context) string {
	if b := s.Repo.HeadBranch(ctx); b != "" {
		return b
	}
	return s.Cfg.Repo.DefaultBranch
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
