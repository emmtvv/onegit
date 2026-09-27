// Command onegit is a single-repository git server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"onegit/internal/auth"
	"onegit/internal/blob"
	"onegit/internal/config"
	"onegit/internal/hooks"
	"onegit/internal/registry"
	"onegit/internal/runner"
	"onegit/internal/server"
	"onegit/internal/store"
)

const usage = `onegit — a single-repository git server

Usage:
  onegit serve                          run the server
  onegit healthcheck                    exit 0 if the local server is healthy
  onegit admin list-users
  onegit admin reset-password -username NAME -password TEMP
                                        set a temporary password (must be changed at next login)
  onegit admin registry-gc [-grace 24h]
                                        delete unreferenced registry blobs and stale uploads

  onegit runner -url URL -token TOKEN [-workdir DIR] [-capacity N]
                                        run CI/deploy jobs (token from Admin → Runners)

  onegit hook <pre-receive|post-receive>   (invoked by git)
  onegit version                        print the version

Configuration comes from ONEGIT_* environment variables, optionally layered
over a YAML file given by -config or ONEGIT_CONFIG.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "healthcheck":
		err = cmdHealthcheck(os.Args[2:])
	case "hook":
		if len(os.Args) != 3 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		os.Exit(hooks.RunHook(os.Args[2], os.Stdin, os.Stdout, os.Stderr))
	case "admin":
		err = cmdAdmin(os.Args[2:])
	case "runner":
		err = cmdRunner(os.Args[2:])
	case "version", "--version":
		fmt.Printf("onegit %s (commit %s)\n", version, commit)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// version and commit are set at build time with
// -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func cmdRunner(args []string) error {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	url := fs.String("url", os.Getenv("ONEGIT_RUNNER_URL"), "onegit base URL (ONEGIT_RUNNER_URL)")
	token := fs.String("token", os.Getenv("ONEGIT_RUNNER_TOKEN"), "runner token (ONEGIT_RUNNER_TOKEN)")
	workdir := fs.String("workdir", envOr("ONEGIT_RUNNER_WORKDIR", "./onegit-runner"), "work directory (ONEGIT_RUNNER_WORKDIR)")
	capacity := fs.Int("capacity", 1, "jobs to run at the same time")
	keep := fs.Bool("keep-workspaces", false, "keep job workspaces after they finish (debugging)")
	fs.Parse(args)

	r, err := runner.New(runner.Config{URL: *url, Token: *token, WorkDir: *workdir, Capacity: *capacity,
		KeepWork: *keep, Version: version, Log: newLogger()})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return r.Run(ctx)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", os.Getenv("ONEGIT_CONFIG"), "optional YAML config file")
}

func newLogger() *slog.Logger {
	var h slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	if os.Getenv("ONEGIT_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	return slog.New(h)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := configFlag(fs)
	fs.Parse(args)

	log := newLogger()
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	log.Info("repository", "path", cfg.RepoPath(), "http", cfg.HTTPCloneURL())
	return srv.Run(ctx)
}

// cmdHealthcheck probes /healthz on the local listener; used by Docker's
// HEALTHCHECK so the image needs no curl.
func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	cfgPath := configFlag(fs)
	path := fs.String("path", "/healthz", "endpoint to probe (/healthz or /readyz)")
	fs.Parse(args)
	addr := ":3000"
	if cfg, err := config.Load(*cfgPath); err == nil {
		addr = cfg.HTTP.Addr
	} else if v := os.Getenv("ONEGIT_HTTP_ADDR"); v != "" {
		addr = v
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + *path)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: %s", resp.Status)
	}
	return nil
}

func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("admin: missing subcommand")
	}
	fs := flag.NewFlagSet("admin "+args[0], flag.ExitOnError)
	cfgPath := configFlag(fs)
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "temporary password")
	grace := fs.Duration("grace", registry.GCGrace, "registry-gc: keep blobs and uploads used within this period")
	fs.Parse(args[1:])

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL(), 2)
	if err != nil {
		return err
	}
	defer st.Close()

	switch args[0] {
	case "reset-password":
		u, err := st.UserByUsername(ctx, *username)
		if err != nil {
			return fmt.Errorf("user %q: %w", *username, err)
		}
		if err := auth.ValidatePassword(*password); err != nil {
			return err
		}
		if u.PasswordHash, err = auth.HashPassword(*password); err != nil {
			return err
		}
		u.MustChangePassword, u.Active = true, true
		if err := st.UpdateUser(ctx, u); err != nil {
			return err
		}
		fmt.Printf("temporary password set for %s; it must be changed at next login\n", u.Username)
	case "registry-gc":
		bs, err := blob.Open(ctx, cfg)
		if err != nil {
			return err
		}
		reg := &registry.Service{Store: st, Blob: bs, Cfg: cfg, Log: newLogger()}
		stats, err := reg.GC(ctx, *grace)
		if err != nil {
			return err
		}
		fmt.Printf("removed %d stale uploads and %d blobs (%d bytes)\n", stats.Uploads, stats.Blobs, stats.BytesFreed)
	case "list-users":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			fmt.Printf("%-4d %-20s %-6s active=%v must_change_password=%v %s\n",
				u.ID, u.Username, u.Role, u.Active, u.MustChangePassword, u.Email)
		}
	default:
		return fmt.Errorf("unknown admin subcommand %q", args[0])
	}
	return nil
}
