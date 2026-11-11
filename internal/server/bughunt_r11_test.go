package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// r11Put sends a node API request on the main listener with a node token.
func r11NodeReq(t *testing.T, env *servertest.Env, token, method, path string, body []byte, hdr ...string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, env.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// r11Login returns a CLI session token from a password login.
func r11Login(t *testing.T, env *servertest.Env, user string) string {
	t.Helper()
	r := env.Do("", "POST", "/auth/login", map[string]any{"username": user, "password": "password-" + user, "client": "cli"})
	if r.Status != 200 || r.Str("token") == "" {
		t.Fatalf("login %s: %s", user, r)
	}
	return r.Str("token")
}

// bugs-r11-sonnet 1: handleBlobPut (nodes.go:~690) stores whatever a node
// PUTs under any snapshot or image id, also over a blob that is stored and
// verified already (store.Blobs.Put: "Existing blobs are replaced"). Any
// node, including a community node of another owner that only learned the id
// of a layer (a node that restores a shared or saved snapshot knows its
// whole chain), can replace the stored disk layer or image that later
// sandboxes of other tenants boot from.
func TestBughuntR11NodeOverwritesStoredSnapshotBlob(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{})
	b := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": a.ID}})
	env.WaitState(key, id, "ready")
	snap, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("layer stored", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id=? AND uploaded_at IS NOT NULL`, snap).Scan(&n)
		return n == 1
	})
	before, err := env.S.Blobs.Stat(store.BlobSnapshot, snap)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := json.Marshal(proto.SnapshotRef{ID: snap, SandboxID: id})
	st, body := r11NodeReq(t, env, b.Token, "PUT", "/node/v1/blobs/snapshot/"+snap, []byte("evil disk layer"), "X-Crowdvm-Snapshot: "+string(ref))
	after, _ := env.S.Blobs.Stat(store.BlobSnapshot, snap)
	if st == 200 || after.SHA256 != before.SHA256 {
		t.Fatalf("node %s replaced the stored layer of a sandbox it does not host (HTTP %d %s): sha %s -> %s", b.ID, st, body, before.SHA256, after.SHA256)
	}
}

// bugs-r11-sonnet 2: the join-token check (handleNodeRegister, nodes.go:~380)
// reads uses and max_uses, and only later runs `uses=uses+1`. Parallel
// registrations all pass the check: a single-use token (the default: maxUses
// is 1) registers as many nodes as the caller can send at once.
func TestBughuntR11SingleUseJoinTokenRegistersManyNodes(t *testing.T) {
	env := servertest.New(t)
	// Timing decides how many get through: try several tokens.
	for round := 0; round < 10; round++ {
		_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "once", CreatedBy: env.Admin.ID, TTL: time.Hour, MaxUses: 1})
		if err != nil {
			t.Fatal(err)
		}
		const n = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				body, _ := json.Marshal(proto.RegisterRequest{JoinToken: tok, Info: proto.NodeInfo{Name: "dup-" + string(rune('a'+i))}})
				<-start
				resp, err := http.Post(env.URL+"/node/v1/register", "application/json", bytes.NewReader(body))
				if err != nil {
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if ok > 1 {
			t.Fatalf("a single-use join token registered %d nodes (round %d)", ok, round)
		}
	}
}

// bugs-r11-sonnet 3: node names are chosen by the node (register and every
// heartbeat) and nothing keeps them unique, yet nodeByRef ("its (unique)
// name") and placement.node pins match by name. A node of another owner that
// names itself like the user's own node attracts the sandboxes pinned to that
// name (whose disks its owner can then read).
func TestBughuntR11NodeNamesAreNotUnique(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Name: "my-laptop"})
	bob, _ := env.NewUser("r11bob", false)
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "bob", CreatedBy: env.Admin.ID, OwnerID: bob.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	env.StartNode(servertest.NodeOptions{Name: "my-laptop", JoinToken: tok})
	names := map[string]int{}
	for _, n := range env.Do(env.AdminKey, "GET", "/api/v1/nodes", nil).Get("nodes").([]any) {
		names[strings.ToLower(n.(map[string]any)["name"].(string))]++
	}
	if names["my-laptop"] > 1 {
		t.Fatalf("two nodes of different owners are both named my-laptop: a pin to that name matches both (%v)", names)
	}
}

// bugs-r11-sonnet 4: handleResume answers the "resume while stopping" case
// (resumeDuringStop, lifecycle_place.go) before it runs checkCredits,
// checkConcurrent and checkStarts, and afterStopResume only records the
// start. A stop followed by a resume during the stop restarts the sandbox
// past the per-minute start limit (and the credit and concurrency gates).
func TestBughuntR11ResumeDuringStopSkipsStartLimit(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.StartsPerMinute = 1 })
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil) // the one start of this minute
	env.WaitState(key, id, "ready")
	node.FailNext("POST /v1/vms/" + id + "/stop")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.stop.status", 5*time.Second, "failing")
	node.Disconnect()
	env.Eventually("node offline", func() bool { return env.S.NodeConn(node.ID) == nil })
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 429 || r.Code() != "rate_limited" {
		t.Fatalf("a second start inside the minute limit of 1 was accepted by a resume during a stop: %s", r)
	}
}

// bugs-r11-sonnet 5: handleCreateSandbox returns a NewSandbox hook error with
// a plain `return err` where every other refusal after idemBegin goes
// through fail(), which releases the Idempotency-Key. The key stays
// "in_progress" for two minutes: the client's retry of the same request gets
// 409 idempotency_in_progress instead of the refusal it would get again.
func TestBughuntR11HookRefusalLeavesIdempotencyKeyBusy(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	sess := env.Session(env.Admin)
	if r := env.Do(sess, "PATCH", "/api/v1/account/data-retention", map[string]any{"snapshotsOff": true}); r.Status != 200 {
		t.Fatalf("retention: %s", r)
	}
	body := map[string]any{"snapshots": true}
	first := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: r11-key")
	if first.Code() != "snapshots_off_for_account" {
		t.Fatalf("first: %s", first)
	}
	second := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: r11-key")
	if second.Code() != "snapshots_off_for_account" {
		t.Fatalf("the retry of a refused request is answered %s instead of the refusal again", second)
	}
}

// bugs-r11-sonnet 6: SetPassword (auth.go:159), used by the password change,
// the admin reset and `crowdvm-server set-password`, leaves every session of
// the account alive. Changing a password after a token leaked (the usual
// reason) does not log the thief out; the stolen CLI token works for the
// whole session TTL.
func TestBughuntR11PasswordChangeKeepsOtherSessions(t *testing.T) {
	env := servertest.New(t)
	env.NewUser("r11carol", false)
	mine := r11Login(t, env, "r11carol")
	stolen := r11Login(t, env, "r11carol")
	r := env.Do(mine, "POST", "/auth/password", map[string]any{"currentPassword": "password-r11carol", "newPassword": "a-new-password-1"})
	if r.Status != 200 {
		t.Fatalf("change password: %s", r)
	}
	if r := env.Do(stolen, "GET", "/auth/me", nil); r.Status == 200 {
		t.Fatalf("a session opened before the password change still works: %s", r)
	}
}

// bugs-r11-sonnet 7: PATCH /auth/me stores any email with no proof the user
// owns it, and email login (handleEmailVerify) signs whoever proves control
// of that address in to the account that holds it. A user who sets their
// email to an address that has no account yet (a colleague, a target) gets
// that person's later "sign in with email" into the user's account: their
// sandboxes, keys and secrets go to the attacker's account.
func TestBughuntR11UnverifiedEmailCapturesLaterEmailLogin(t *testing.T) {
	var mu sync.Mutex
	var mailed string
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	env.S.Hooks.Mail = func(to, subject, body string) { mu.Lock(); mailed = body; mu.Unlock() }
	env.NewUser("r11mallory", false)
	mal := r11Login(t, env, "r11mallory")
	if r := env.Do(mal, "PATCH", "/auth/me", map[string]any{"email": "ceo@corp.test"}); r.Status != 200 {
		t.Fatalf("set email: %s", r)
	}
	if r := env.Do("", "POST", "/auth/email/start", map[string]any{"email": "ceo@corp.test"}); r.Status != 200 {
		t.Fatalf("start: %s", r)
	}
	mu.Lock()
	code := regexp.MustCompile(`\d{6}`).FindString(mailed)
	mu.Unlock()
	r := env.Do("", "POST", "/auth/email/verify", map[string]any{"email": "ceo@corp.test", "code": code, "client": "cli"})
	if r.Status == 200 && r.Str("user.login") == "r11mallory" {
		t.Fatalf("the owner of ceo@corp.test signed in to r11mallory's account, which claimed the address without proof: %s", r)
	}
}

// bugs-r11-sonnet 8: handleEmailVerify reads attempts, checks it is < 5 and
// only then increments it (two statements, no atomic guard). Parallel guesses
// all see attempts=0: the 5-attempt limit does not hold, so the 6-digit code
// can be tried far more than 5 times per code.
func TestBughuntR11EmailCodeAttemptLimitRaces(t *testing.T) {
	var mu sync.Mutex
	var mailed string
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	env.S.Hooks.Mail = func(to, subject, body string) { mu.Lock(); mailed = body; mu.Unlock() }
	// Timing decides how many guesses overlap: try several codes.
	for round := 0; round < 8; round++ {
		if r := env.Do("", "POST", "/auth/email/start", map[string]any{"email": "race@x.test"}); r.Status != 200 {
			t.Fatalf("start: %s", r)
		}
		mu.Lock()
		code := regexp.MustCompile(`\d{6}`).FindString(mailed)
		mu.Unlock()
		wrong := "000000"
		if code == wrong {
			wrong = "000001"
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 60; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				body, _ := json.Marshal(map[string]any{"email": "race@x.test", "code": wrong})
				resp, err := http.Post(env.URL+"/auth/email/verify", "application/json", bytes.NewReader(body))
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
		}
		close(start)
		wg.Wait()
		var attempts int
		_ = env.S.DB.QueryRow(`SELECT attempts FROM email_codes WHERE email='race@x.test'`).Scan(&attempts)
		if attempts > 5 {
			t.Fatalf("%d wrong codes were evaluated against one code, the limit is 5 (round %d)", attempts, round)
		}
	}
}

// bugs-r11-sonnet 9: covers() (apikeys.go) lets a key mint any action it
// "Allows", and Allows("sandbox.delete") is true for a key that only has
// "sandbox.delete-own" (apikeys.go: `case "sandbox.delete": return
// k.hasAction("sandbox.delete-own")`, meant to be narrowed per sandbox). A key
// that can manage keys (account.admin) and may delete only sandboxes it
// created therefore creates a child key with plain sandbox.delete, which
// deletes any sandbox of the account.
func TestBughuntR11DeleteOwnKeyMintsDeleteKey(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	sess := env.Session(env.Admin)
	r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "narrow", "actions": []string{"account.admin", "sandbox.delete-own", "sandbox.read"}})
	if r.Status != 201 {
		t.Fatalf("create narrow key: %s", r)
	}
	narrow := r.Str("secret")
	victim := createSandbox(t, env, env.AdminKey, nil) // not created by the narrow key
	env.WaitState(env.AdminKey, victim, "ready")
	child := env.Do(narrow, "POST", "/api/v1/api-keys", map[string]any{"name": "child", "ttl": "1d", "actions": []string{"sandbox.delete", "sandbox.read"}})
	if child.Status == 201 {
		del := env.Do(child.Str("secret"), "DELETE", "/api/v1/sandboxes/"+victim, nil, "X-CrowdVM-Confirm-Delete: "+victim)
		t.Fatalf("a key limited to deleting its own sandboxes minted a key with sandbox.delete (delete of another sandbox: %d)", del.Status)
	}
}

// bugs-r11-sonnet 10: bcrypt refuses passwords over 72 bytes
// (ErrPasswordTooLong in the pinned x/crypto). CreateUser and SetPassword
// pass the error up: signup, password change and admin reset answer 500
// internal_error for a long passphrase, where the rule the user can read is
// "at least 8 characters" (weak_password is the only 400 they know).
func TestBughuntR11LongPasswordIsAnInternalError(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	long := strings.Repeat("correct horse battery staple ", 4)
	r := env.Do("", "POST", "/auth/signup", map[string]any{"username": "r11long", "password": long})
	if r.Status >= 500 {
		t.Fatalf("signup with a %d byte passphrase: %s", len(long), r)
	}
}

// bugs-r11-sonnet 11: handleNodeRegister takes over an existing node when
// the request names its id (Info.NodeID) and the join token has the same
// owner. Tokens the admin makes all have the admin as owner, so a contributor
// holding any such token can register as any other node of the pool (node ids
// are listed to every user): the server then re-keys that node (the real node's
// token stops working), and the sandboxes and snapshot locations of that node
// id now point at the contributor's machine.
func TestBughuntR11RegisterTakesOverAnotherNode(t *testing.T) {
	env := servertest.New(t)
	victim := env.StartNode(servertest.NodeOptions{Name: "victim-node"})
	tok := env.JoinToken() // another token of the same owner
	body, _ := json.Marshal(proto.RegisterRequest{JoinToken: tok, Info: proto.NodeInfo{NodeID: victim.ID, Name: "evil"}})
	resp, err := http.Post(env.URL+"/node/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rr proto.RegisterResponse
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	if resp.StatusCode == 200 && rr.NodeID == victim.ID {
		st, _ := r11NodeReq(t, env, victim.Token, "GET", "/node/v1/info", nil)
		t.Fatalf("a node registered with another token took over node %s; the real node's token now answers HTTP %d", victim.ID, st)
	}
}

// bugs-r11-sonnet 12: recordTaken (snapshots.go:~178) answers a node's
// "I took layer X" for a layer the server already knows by adding that node
// as a location of X, with no check that the node took it (snapshots.node_id)
// or hosts the sandbox. Any node that knows a layer id becomes a recorded
// holder: scheduling prefers it for restores, chainReachable counts it as
// holding unstored layers, and deletions of that layer wait for it to erase a
// copy it never had.
func TestBughuntR11NodeClaimsHoldingForeignLayer(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{})
	b := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": a.ID}})
	env.WaitState(key, id, "ready")
	snap, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := json.Marshal(proto.SnapshotRef{ID: snap, SandboxID: id})
	st, _ := r11NodeReq(t, env, b.Token, "POST", "/node/v1/snapshots", ref)
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshot_locations WHERE snapshot_id=? AND node_id=?`, snap, b.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("node %s (HTTP %d) is recorded as a holder of layer %s of a sandbox on node %s", b.ID, st, snap, a.ID)
	}
}

// bugs-r11-sonnet 13: onSnapshotUploaded (snapshots.go:~255) runs
// `UPDATE snapshots SET size_bytes=?` from the event of whichever node sends
// it, for any snapshot id, with only the layer's existence checked. Another
// node can set the size of a layer it neither took nor holds; sizes feed the
// saved-snapshot allowance and storage numbers of the owner.
func TestBughuntR11NodeSetsSizeOfForeignLayer(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{})
	b := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": a.ID}})
	env.WaitState(key, id, "ready")
	snap, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("layer stored", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE id=? AND uploaded_at IS NOT NULL`, snap).Scan(&n)
		return n == 1
	})
	var before int64
	_ = env.S.DB.QueryRow(`SELECT size_bytes FROM snapshots WHERE id=?`, snap).Scan(&before)
	data, _ := json.Marshal(proto.SnapshotRef{ID: snap, SandboxID: id, SizeBytes: 1 << 40})
	evs, _ := json.Marshal([]proto.Event{{Time: time.Now(), SandboxID: id, Type: proto.EvSnapshotUploaded, Data: data}})
	r11NodeReq(t, env, b.Token, "POST", "/node/v1/events", evs, "Content-Type: application/json")
	var after int64
	_ = env.S.DB.QueryRow(`SELECT size_bytes FROM snapshots WHERE id=?`, snap).Scan(&after)
	if after != before {
		t.Fatalf("node %s changed the size of node %s's layer from %d to %d", b.ID, a.ID, before, after)
	}
}

// bugs-r11-sonnet 14: defaultImage (scheduler.go:~323) is the image row with
// the newest created_at, and handleBlobPut writes that row for whatever image
// id any node PUTs (also over an existing one, with created_at = now). When
// no --default-image is configured, a node of any owner that uploads an image
// makes it the base image of every new sandbox of every tenant.
func TestBughuntR11AnyNodeUploadBecomesDefaultImage(t *testing.T) {
	env := servertest.New(t)
	admin := env.StartNode(servertest.NodeOptions{})
	bob, _ := env.NewUser("r11dave", false)
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "dave", CreatedBy: env.Admin.ID, OwnerID: bob.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	other := env.StartNode(servertest.NodeOptions{JoinToken: tok})
	if st, b := r11NodeReq(t, env, admin.Token, "PUT", "/node/v1/blobs/image/base-v1", []byte("the real base image")); st != 200 {
		t.Fatalf("admin image: %d %s", st, b)
	}
	time.Sleep(5 * time.Millisecond)
	// The fix refuses images from untrusted nodes of other owners (403); a
	// stored one would also do if it never became the default.
	if st, b := r11NodeReq(t, env, other.Token, "PUT", "/node/v1/blobs/image/evil-v1", []byte("backdoored image")); st != 200 && st != 403 {
		t.Fatalf("other image: %d %s", st, b)
	}
	_, body := r11NodeReq(t, env, admin.Token, "GET", "/node/v1/images", nil)
	var out struct {
		Default string `json:"default"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Default == "evil-v1" {
		t.Fatalf("an image uploaded by a node of another owner is the default image for every new sandbox: %s", body)
	}
}

// bugs-r11-sonnet 15: UserByLogin (auth.go:~80) matches `username=? OR email=?
// OR id=? LIMIT 1` with no priority, and a username may look like an id
// (letters, digits, underscore: "usr_" + 16 characters is valid). A user who
// registers the username usr_<victim id> (ids are listed in org member
// lists) squats every lookup by that id: credit transfers, org invites and
// owner assignments that name the victim by id, and the password login.
func TestBughuntR11UsernameCanBeAnotherUsersID(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	victim, _ := env.NewUser("r11victim", false)
	r := env.Do("", "POST", "/auth/signup", map[string]any{"username": victim.ID, "password": "squatter-password"})
	if r.Status == 200 {
		u, err := env.S.UserByLogin(victim.ID)
		if err != nil || u.ID != victim.ID {
			t.Fatalf("UserByLogin(%q) returns another account: %+v %v", victim.ID, u, err)
		}
		t.Fatalf("a username equal to another user's id was accepted (lookup by id happens to return the right row here)")
	}
}
