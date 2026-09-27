package auth_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"onegit/internal/auth"
	"onegit/internal/store"
	"onegit/internal/testutil"
)

var ctx = context.Background()

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                 false,
		"exactly10!":            true,
		"пароль-юникод":         true, // counted in runes
		"пароль123":             false,
		strings.Repeat("a", 72): true,
		strings.Repeat("a", 73): false, // bcrypt limit
	} {
		if err := auth.ValidatePassword(pw); (err == nil) != ok {
			t.Errorf("ValidatePassword(%q) = %v", pw, err)
		}
	}
}

func TestHashes(t *testing.T) {
	h, err := auth.HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "$2") {
		t.Fatalf("HashPassword = %q, %v", h, err)
	}
	if a, b := auth.RandomString(16), auth.RandomString(16); a == b || len(a) != 22 {
		t.Errorf("RandomString: %q, %q", a, b)
	}
	if a, b := auth.HashToken("x"), auth.HashToken("x"); a != b || a == auth.HashToken("y") || len(a) != 64 {
		t.Error("HashToken is not a stable sha256 hex")
	}
}

func TestRequestIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	if got := auth.RequestIP(r, false); got != "10.0.0.1" {
		t.Errorf("without trust = %q", got)
	}
	if got := auth.RequestIP(r, true); got != "2.2.2.2" {
		t.Errorf("X-Forwarded-For: last hop expected, got %q", got)
	}
	r.Header.Set("X-Real-IP", " 3.3.3.3 ")
	if got := auth.RequestIP(r, true); got != "3.3.3.3" {
		t.Errorf("X-Real-IP = %q", got)
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "unix-socket"
	if got := auth.RequestIP(r, true); got != "unix-socket" {
		t.Errorf("no port = %q", got)
	}
}

func newService(t *testing.T) (*auth.Service, *store.Store) {
	cfg := testutil.Config(t)
	st := testutil.Store(t, cfg)
	return &auth.Service{Store: st, KV: testutil.KV(t, cfg)}, st
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	alice := testutil.User(t, st, "alice", store.RoleWrite)

	u, err := s.CheckPassword(ctx, "ALICE", testutil.Password("alice"), "1.2.3.4")
	if err != nil || u.ID != alice.ID {
		t.Fatalf("login (case-insensitive username) = %v, %v", u, err)
	}
	for _, c := range []struct{ user, pw string }{{"alice", "wrong-password"}, {"nobody", "whatever-pass"}} {
		if _, err := s.CheckPassword(ctx, c.user, c.pw, "1.2.3.4"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("%s/%s: %v", c.user, c.pw, err)
		}
	}

	// Inactive users and users without a password cannot log in.
	alice.Active = false
	testutil.Must(t, st.UpdateUser(ctx, alice))
	if _, err := s.CheckPassword(ctx, "alice", testutil.Password("alice"), "9.9.9.9"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("inactive user: %v", err)
	}
	sso := testutil.User(t, st, "sso", store.RoleRead)
	sso.PasswordHash = ""
	testutil.Must(t, st.UpdateUser(ctx, sso))
	if _, err := s.CheckPassword(ctx, "sso", "", "9.9.9.9"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("user without password: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	testutil.User(t, st, "bob", store.RoleRead)
	for i := 0; i < 10; i++ {
		if _, err := s.CheckPassword(ctx, "bob", "wrong-password", "5.5.5.5"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Locked for this IP, even with the right password...
	if _, err := s.CheckPassword(ctx, "bob", testutil.Password("bob"), "5.5.5.5"); !errors.Is(err, auth.ErrTooManyAttempts) {
		t.Errorf("11th attempt: %v", err)
	}
	// ...but not for the same user from elsewhere.
	if _, err := s.CheckPassword(ctx, "bob", testutil.Password("bob"), "6.6.6.6"); err != nil {
		t.Errorf("other IP locked out: %v", err)
	}
	// A successful login resets the counter.
	s2, st2 := newService(t)
	testutil.User(t, st2, "carol", store.RoleRead)
	for i := 0; i < 9; i++ {
		s2.CheckPassword(ctx, "carol", "wrong-password", "7.7.7.7")
	}
	if _, err := s2.CheckPassword(ctx, "carol", testutil.Password("carol"), "7.7.7.7"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		s2.CheckPassword(ctx, "carol", "wrong-password", "7.7.7.7")
	}
	if _, err := s2.CheckPassword(ctx, "carol", testutil.Password("carol"), "7.7.7.7"); err != nil {
		t.Errorf("counter not reset by a successful login: %v", err)
	}
}

func TestCheckBasicAndTokens(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	dave := testutil.User(t, st, "dave", store.RoleWrite)

	if u, err := s.CheckBasic(ctx, "dave", testutil.Password("dave"), "1.1.1.1"); err != nil || u.ID != dave.ID {
		t.Errorf("basic with password: %v, %v", u, err)
	}
	plain, tok, err := s.NewToken(ctx, dave.ID, "laptop")
	if err != nil || !strings.HasPrefix(plain, auth.TokenPrefix) || !strings.HasPrefix(plain, tok.Prefix) || tok.ID == 0 {
		t.Fatalf("NewToken = %q, %+v, %v", plain, tok, err)
	}
	// The username is ignored with a token.
	if u, err := s.CheckBasic(ctx, "anything", plain, "1.1.1.1"); err != nil || u.ID != dave.ID {
		t.Errorf("basic with token: %v, %v", u, err)
	}
	if u, err := s.CheckToken(ctx, plain); err != nil || u.ID != dave.ID {
		t.Errorf("CheckToken: %v, %v", u, err)
	}
	toks, _ := st.ListTokens(ctx, dave.ID)
	if len(toks) != 1 || toks[0].LastUsed == nil {
		t.Errorf("token last_used not recorded: %+v", toks)
	}
	if _, err := s.CheckToken(ctx, auth.TokenPrefix+"forged"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("forged token: %v", err)
	}

	// A temporary password works in the UI only.
	dave.MustChangePassword = true
	testutil.Must(t, st.UpdateUser(ctx, dave))
	if _, err := s.CheckBasic(ctx, "dave", testutil.Password("dave"), "1.1.1.1"); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Errorf("basic with a temporary password: %v", err)
	}
	if _, err := s.CheckPassword(ctx, "dave", testutil.Password("dave"), "1.1.1.1"); err != nil {
		t.Errorf("UI login with a temporary password: %v", err)
	}

	// Deactivated users lose token access; deleted tokens stop working.
	dave.Active = false
	testutil.Must(t, st.UpdateUser(ctx, dave))
	if _, err := s.CheckToken(ctx, plain); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("token of an inactive user: %v", err)
	}
	dave.Active = true
	testutil.Must(t, st.UpdateUser(ctx, dave))
	testutil.Must(t, st.DeleteToken(ctx, dave.ID, tok.ID))
	if _, err := s.CheckToken(ctx, plain); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("deleted token: %v", err)
	}
}

func TestSessions(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	eve := testutil.User(t, st, "eve", store.RoleRead)
	id, err := s.NewSession(ctx, eve.ID)
	if err != nil || len(id) < 40 {
		t.Fatalf("NewSession = %q, %v", id, err)
	}
	if u, err := s.SessionUser(ctx, id); err != nil || u.ID != eve.ID {
		t.Errorf("SessionUser = %v, %v", u, err)
	}
	if _, err := s.SessionUser(ctx, "made-up"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("unknown session: %v", err)
	}
	eve.Active = false
	testutil.Must(t, st.UpdateUser(ctx, eve))
	if _, err := s.SessionUser(ctx, id); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("session of an inactive user: %v", err)
	}
	testutil.Must(t, s.EndSession(ctx, id))
	eve.Active = true
	testutil.Must(t, st.UpdateUser(ctx, eve))
	if _, err := s.SessionUser(ctx, id); err == nil {
		t.Error("ended session still valid")
	}
}

func TestSetPassword(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	u := testutil.User(t, st, "frank", store.RoleRead)
	u.MustChangePassword = true
	testutil.Must(t, st.UpdateUser(ctx, u))
	if err := s.SetPassword(ctx, u, "short"); err == nil {
		t.Error("short password accepted")
	}
	testutil.Must(t, s.SetPassword(ctx, u, "a brand new password"))
	got, _ := st.UserByID(ctx, u.ID)
	if got.MustChangePassword {
		t.Error("must_change_password not cleared")
	}
	if _, err := s.CheckBasic(ctx, "frank", "a brand new password", "1.1.1.1"); err != nil {
		t.Errorf("new password: %v", err)
	}
}

func TestJobTokens(t *testing.T) {
	t.Parallel()
	s, st := newService(t)
	trigger := testutil.User(t, st, "grace", store.RoleAdmin)
	run := &store.Run{Kind: "pipeline", Name: "ci", File: "ci.yml", Event: "push", Ref: "refs/heads/main",
		SHA: strings.Repeat("a", 40), TriggeredBy: &trigger.ID, Status: store.JobQueued}
	job := &store.Job{Name: "build", Kind: "ci", RunsOn: []string{}, Spec: []byte("{}"), Status: store.JobQueued}
	testutil.Must(t, st.CreateRun(ctx, run, []*store.Job{job}))
	runner := &store.Runner{Name: "r1", Kind: "ci", Labels: []string{"linux"}}
	testutil.Must(t, st.CreateRunner(ctx, runner, auth.HashToken("ogrun_x")))

	plain := auth.JobTokenPrefix + auth.RandomString(20)
	claimed, err := st.ClaimJob(ctx, runner, auth.HashToken(plain), func(*store.Job) bool { return true })
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("ClaimJob = %+v, %v", claimed, err)
	}
	u, err := s.CheckBasic(ctx, "ci", plain, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	// The job acts with read access, never with its trigger's admin role.
	if u.Role != store.RoleRead || u.IsAdmin() || u.Job == nil || u.Job.JobID != job.ID || u.Job.SHA != run.SHA ||
		u.Job.Kind != "ci" || *u.Job.UserID != trigger.ID {
		t.Errorf("job user = %+v, job %+v", u, u.Job)
	}
	if ju, err := s.JobUser(ctx, job.ID); err != nil || ju.Job.JobID != job.ID {
		t.Errorf("JobUser = %+v, %v", ju, err)
	}
	if _, err := s.CheckToken(ctx, auth.JobTokenPrefix+"forged"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("forged job token: %v", err)
	}
	// A finished job's token is dead.
	if _, _, err := st.FinishJob(ctx, job.ID, store.JobSuccess, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckToken(ctx, plain); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("token of a finished job: %v", err)
	}
	if _, err := s.JobUser(ctx, job.ID); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("JobUser of a finished job: %v", err)
	}
	if _, err := s.JobUser(ctx, 999999); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("JobUser of a missing job: %v", err)
	}
}
