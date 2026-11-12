package server_test

import (
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r11-sonnet 7: an email is stored only after its code is entered.
func TestFixR11EmailChangeNeedsCode(t *testing.T) {
	var mu sync.Mutex
	var mailed string
	env := servertest.New(t)
	env.S.Hooks.Mail = func(to, subject, body string) { mu.Lock(); mailed = body; mu.Unlock() }
	env.NewUser("r11dave", false)
	tok := r11Login(t, env, "r11dave")
	if r := env.Do(tok, "PATCH", "/auth/me", map[string]any{"email": "Dave@Corp.test"}); r.Status != 200 || r.Str("user.email") == "dave@corp.test" {
		t.Fatalf("patch: %s", r)
	}
	mu.Lock()
	code := regexp.MustCompile(`\d{6}`).FindString(mailed)
	mu.Unlock()
	if code == "" {
		t.Fatal("no code mailed")
	}
	if r := env.Do(tok, "POST", "/auth/me/email/verify", map[string]any{"email": "dave@corp.test", "code": "x" + code}); r.Status != 401 {
		t.Fatalf("wrong code: %s", r)
	}
	if r := env.Do(tok, "POST", "/auth/me/email/verify", map[string]any{"email": "dave@corp.test", "code": code}); r.Status != 200 || r.Str("user.email") != "dave@corp.test" {
		t.Fatalf("verify: %s", r)
	}
}

// bugs-r11-sonnet 6: the caller's own session survives a password change.
func TestFixR11PasswordChangeKeepsCurrentSession(t *testing.T) {
	env := servertest.New(t)
	env.NewUser("r11erin", false)
	mine := r11Login(t, env, "r11erin")
	if r := env.Do(mine, "POST", "/auth/password", map[string]any{"currentPassword": "password-r11erin", "newPassword": "a-new-password-1"}); r.Status != 200 {
		t.Fatalf("change: %s", r)
	}
	if r := env.Do(mine, "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("current session ended: %s", r)
	}
}

// bugs-r11-sonnet 10: a long password is a 400 on change and admin reset.
func TestFixR11LongPasswordIs400(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("r11frank", false)
	tok := r11Login(t, env, "r11frank")
	long := strings.Repeat("x", 73)
	if r := env.Do(tok, "POST", "/auth/password", map[string]any{"currentPassword": "password-r11frank", "newPassword": long}); r.Status != 400 {
		t.Fatalf("change: %s", r)
	}
	adm := env.Session(env.Admin)
	if r := env.Do(adm, "PATCH", "/api/v1/admin/users/"+u.ID, map[string]any{"password": long}); r.Status != 400 {
		t.Fatalf("reset: %s", r)
	}
}

// bugs-r11-sonnet 15: a username shaped like an id is refused, in any case.
func TestFixR11IDShapedUsernameRefused(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	r := env.Do("", "POST", "/auth/signup", map[string]any{"username": "USR_abcdefghijklmnop", "password": "password-abc"})
	if r.Status != 400 || r.Code() != "invalid_username" {
		t.Fatalf("signup: %s", r)
	}
}
