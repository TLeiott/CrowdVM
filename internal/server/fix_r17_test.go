package server_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

const (
	r17KeyAlice = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAliceAliceAliceAliceAliceAliceAlice1"
	r17KeyBob   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBobBobBobBobBobBobBobBobBobBobBobBo2"
	r17KeyCarol = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICarolCarolCarolCarolCarolCarolCa3"
)

func r17AuthorizedKeys(env *servertest.Env, key, sb string) string {
	return string(env.Do(key, "GET", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", nil).Body)
}

// bugs-r17-opus 2: removing a member revokes the keys they sent into the
// org's shared sandboxes (running: at once; stopped: on the next ready).
func TestFixR17RemovedMemberKeysRevoked(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	bob, bobKey := env.NewUser("bob", false)
	carol, carolKey := env.NewUser("carol", false)
	env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	for _, k := range []string{bobKey, carolKey} {
		code := env.Do(alice, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "x@example.test"}).Str("code")
		if r := env.Do(k, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code}); r.Status != 200 {
			t.Fatalf("accept: %s", r)
		}
	}
	running := createSandbox(t, env, alice, map[string]any{"org": "acme", "noEnv": true, "visibility": "shared"})
	stopped := createSandbox(t, env, alice, map[string]any{"org": "acme", "noEnv": true, "visibility": "shared"})
	env.WaitState(alice, running, "ready")
	env.WaitState(alice, stopped, "ready")
	for _, sb := range []string{running, stopped} {
		env.Do(alice, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": r17KeyAlice})
		env.Do(bobKey, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": r17KeyBob})
		env.Do(carolKey, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": r17KeyCarol})
		all := r17KeyAlice + " alice\n" + r17KeyBob + " bob\n" + r17KeyCarol + " carol\n"
		if r := env.Do(alice, "PUT", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", all); r.Status != 200 {
			t.Fatalf("write keys: %s", r)
		}
	}
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+stopped+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(alice, stopped, "archived")

	if r := env.Do(alice, "DELETE", "/api/v1/orgs/acme/members/"+bob.ID, nil); r.Status != 200 {
		t.Fatalf("remove bob: %s", r)
	}
	env.Eventually("bob's key left the running sandbox", func() bool {
		return !strings.Contains(r17AuthorizedKeys(env, alice, running), "IBobBob")
	})
	got := r17AuthorizedKeys(env, alice, running)
	if !strings.Contains(got, "IAliceAlice") || !strings.Contains(got, "ICarolCarol") {
		t.Fatalf("the creator's and the remaining member's keys must stay: %q", got)
	}
	// The stopped sandbox is marked and revoked when it is next ready.
	if s, _ := env.S.SandboxByID(stopped); s == nil || s.Extra["sshKeysRevokePending"] != true {
		t.Fatalf("stopped sandbox is not marked for revocation: %+v", s)
	}
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+stopped+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	env.WaitState(alice, stopped, "ready")
	env.Eventually("bob's key left the resumed sandbox", func() bool {
		return !strings.Contains(r17AuthorizedKeys(env, alice, stopped), "IBobBob")
	})
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_ssh_keys WHERE user_id=?`, bob.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d sandbox_ssh_keys rows of the removed member are left", n)
	}

	// A member who leaves on their own is revoked the same way.
	if r := env.Do(carolKey, "DELETE", "/api/v1/orgs/acme/members/me", nil); r.Status != 200 {
		t.Fatalf("carol leaves: %s", r)
	}
	env.Eventually("carol's key left", func() bool {
		return !strings.Contains(r17AuthorizedKeys(env, alice, running), "ICarolCarol")
	})
	_ = carol
}

// bugs-r17-opus 3: the members endpoint refuses a service account.
func TestFixR17MembersEndpointRefusesServiceAccount(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	env.Do(admin, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	r := env.Do(admin, "POST", "/api/v1/orgs/acme/service-accounts", map[string]any{"name": "ci3", "ttl": "1h"})
	if r.Status != 201 {
		t.Fatalf("service account: %s", r)
	}
	svcID := env.Do(r.Str("secret"), "GET", "/api/v1/me", nil).Str("user.id")
	r = env.Do(admin, "DELETE", "/api/v1/orgs/acme/members/"+svcID, nil)
	if r.Status != 409 || !strings.Contains(string(r.Body), "service-accounts") {
		t.Fatalf("member delete of a service account: %s", r)
	}
	if r := env.Do(admin, "DELETE", "/api/v1/orgs/acme/service-accounts/"+svcID, nil); r.Status != 200 {
		t.Fatalf("its own endpoint still works: %s", r)
	}
}

// bugs-r17-opus 4: with no "from", credits transfer takes from the scope.
func TestFixR17CreditTransferUsesScope(t *testing.T) {
	env := servertest.New(t)
	owner, sess := env.NewUser("owner17", false)
	dave, _ := env.NewUser("dave17", false)
	org := env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "acme17"}).Str("org.id")
	set := func(id string, v float64) {
		_, _ = env.S.DB.Exec(`INSERT INTO wallets(id,balance,updated_at) VALUES(?,?,0) ON CONFLICT(id) DO UPDATE SET balance=excluded.balance`, id, v)
	}
	set(org, 50)
	set(owner.ID, 50)
	set(dave.ID, 0)
	if r := env.Do(sess, "POST", "/api/v1/credits/transfer", map[string]any{"to": "dave17", "amount": 5}, "X-CrowdVM-Org: acme17"); r.Status != 200 {
		t.Fatalf("transfer in org scope: %s", r)
	}
	if o, p, d := env.S.Balance(org), env.S.Balance(owner.ID), env.S.Balance(dave.ID); o != 45 || p != 50 || d != 5 {
		t.Fatalf("org/personal/dave = %v/%v/%v, want 45/50/5", o, p, d)
	}
	// An explicit "personal" stays personal, also inside an org scope.
	if r := env.Do(sess, "POST", "/api/v1/credits/transfer", map[string]any{"from": "personal", "to": "dave17", "amount": 5}, "X-CrowdVM-Org: acme17"); r.Status != 200 {
		t.Fatalf("explicit personal: %s", r)
	}
	if o, p := env.S.Balance(org), env.S.Balance(owner.ID); o != 45 || p != 45 {
		t.Fatalf("org/personal = %v/%v, want 45/45", o, p)
	}
}

// bugs-r17-opus 6: parallel accepts of one invite let one person join.
func TestFixR17InviteAcceptedOnce(t *testing.T) {
	env := servertest.New(t)
	alice := env.AdminKey
	env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	for round := 0; round < 10; round++ {
		code := env.Do(alice, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "x@example.test"}).Str("code")
		var keys []string
		for i := 0; i < 4; i++ {
			_, k := env.NewUser("u"+strings.Repeat("x", round)+string(rune('a'+i)), false)
			keys = append(keys, k)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		for _, k := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r := env.Do(k, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code}); r.Status == 200 {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if ok != 1 {
			t.Fatalf("round %d: %d accepts of one invite succeeded, want 1", round, ok)
		}
	}
}
