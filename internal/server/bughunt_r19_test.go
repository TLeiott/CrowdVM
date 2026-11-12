package server_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/hashicorp/yamux"
)

// r19Confirm is the guest's POST /v1/rekeyed over an open channel.
func r19Confirm(t *testing.T, sess *yamux.Session, dataKey string) int {
	t.Helper()
	raw, _ := base64.StdEncoding.DecodeString(dataKey)
	h := sha256.Sum256(raw)
	req, _ := http.NewRequest("POST", "http://server/v1/rekeyed", strings.NewReader(`{"keyHash":"`+hex.EncodeToString(h[:])+`"}`))
	resp, err := (&http.Client{Transport: mux.Transport(sess)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Round 18 refuses every copy of a snapshot whose row carries a pending old
// key (rekey_from) with a "retry in a few minutes" 409. A snapshot row keeps
// that value for ever (diskRekeyed clears only the sandbox and its channel),
// so a saved snapshot that was saved while its sandbox was still re-encrypting
// can never be deployed or shared-deployed again, however long the owner
// waits: the refusal is marked retryable and is not.
func TestBughuntR19SavedSnapshotOfRekeyingCopyIsRefusedForEver(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	src := createSandbox(t, env, key, map[string]any{"type": "small"})
	env.WaitState(key, src, "ready")
	spec, _ := node.Spec(src)
	rawChannel(t, env, src, spec.Guest.Channel.Ticket)
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "r19tpl"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/r19tpl", "snapshot.status", 5*time.Second, "ready")

	cp := createSandbox(t, env, key, map[string]any{"from": "r19tpl", "newDiskKey": true})
	env.WaitState(key, cp, "ready")
	spec, _ = node.Spec(cp)
	sess, w := rawChannel(t, env, cp, spec.Guest.Channel.Ticket)
	if w.OldDataKey == "" {
		t.Fatal("setup: the copy has no pending key")
	}
	// The owner saves the copy while it re-encrypts.
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": cp, "name": "r19mid"}); r.Status != 202 {
		t.Fatalf("save mid: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/r19mid", "snapshot.status", 5*time.Second, "ready")
	// The copy's guest finishes and confirms.
	if code := r19Confirm(t, sess, w.DataKey); code != http.StatusNoContent {
		t.Fatalf("confirm: %d", code)
	}
	time.Sleep(300 * time.Millisecond)

	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "r19mid"})
	if r.Status == http.StatusConflict && r.Str("error.code") == "disk_rekey_pending" {
		t.Fatalf("a snapshot saved during the re-encryption is refused as 'retry in a few minutes' even after the source confirmed: %s", r)
	}
}

// The Box alias (feat_legacy.go) rewrites the body of every request whose
// Content-Type mentions json, also a raw file upload: the bytes of a JSON file
// are parsed, renamed (a key "box" becomes "sandbox", "boxes" "sandboxes")
// and re-encoded. The file in the sandbox is not what the client sent.
func TestBughuntR19BoxAliasRewritesRawFileUploads(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	const doc = `{ "box": "mine",  "boxes": [1, 2] }` // a user's own file
	put := func(base string) string {
		if r := env.Do(key, "PUT", base+"/sandboxes/"+id+"/files/raw?path=/home/user/x.json", doc); r.Status != 200 {
			t.Fatalf("put %s: %s", base, r)
		}
		return string(env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/files/raw?path=/home/user/x.json", nil).Body)
	}
	if got := put("/api/v1"); got != doc {
		t.Fatalf("native upload altered the file: %q", got)
	}
	if got := put("/api/box/v1"); got != doc {
		t.Fatalf("an upload through /api/box/v1 changed the file's bytes: got %q, sent %q", got, doc)
	}
}

// The alias wraps every body in an 8 MiB MaxBytesReader, also for streams the
// native route passes through to the sandbox untouched: a 10 MiB raw upload
// works on /api/v1 and fails on /api/box/v1.
func TestBughuntR19BoxAliasCapsRawUploads(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	big := make([]byte, 10<<20)
	for _, base := range []string{"/api/v1", "/api/box/v1"} {
		r := env.Do(key, "PUT", base+"/sandboxes/"+id+"/files/raw?path=/home/user/big.bin", big, "Content-Type: application/octet-stream")
		if r.Status != 200 {
			t.Fatalf("%s: a 10 MiB raw upload answered %d: %.200s", base, r.Status, r.Body)
		}
		if n, _ := r.Get("size").(float64); int(n) != len(big) {
			t.Fatalf("%s: the sandbox got %v bytes of %d", base, r.Get("size"), len(big))
		}
	}
}

// A saved snapshot's layer stays readable by its saver after its sandbox is
// deleted (snapForRead, SNAP-032), and GET /snapshots/{id}/download then hands
// out the inventory link of the manifest: but the link's own handler answers
// 404 for any snapshot with deleted_at set, so the manifest advertises a
// link that cannot work.
func TestBughuntR19SavedSnapshotInventoryLinkWorks(t *testing.T) {
	env := servertest.New(t)
	fi := newFakeInspector()
	env.StartNode(servertest.NodeOptions{Routes: fi.routes})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "r19inv"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/r19inv", "snapshot.status", 5*time.Second, "ready")
	snap := env.Do(key, "GET", "/api/v1/saved-snapshots/r19inv", nil).Str("snapshot.snapshotId")
	if snap == "" {
		t.Fatal("no snapshot id")
	}
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id); r.Status/100 != 2 {
		t.Fatalf("delete: %s", r)
	}
	env.Eventually("source deleted", func() bool {
		sn, err := env.S.SnapshotByID(snap)
		return err == nil && sn.DeletedAt != nil
	})
	r := env.Do(key, "GET", "/api/v1/snapshots/"+snap+"/download", nil)
	if r.Status != 200 {
		t.Skipf("download of the saved layer: %s", r)
	}
	inv := r.Str("inventory.signedUrl")
	resp := hget(t, env, "", strings.TrimPrefix(inv, env.S.Cfg.PublicURL))
	if resp.StatusCode != 200 {
		t.Fatalf("the manifest's inventory link answers %d: %.200s", resp.StatusCode, resp.body)
	}
}

// POST /account/erase deletes the account's org_members rows without looking
// at roles. The owner of an organization that still has members erases
// their account: the organization is left with no owner. Nobody can rename
// it, delete it, move its credits out, or promote anyone any more (every one
// of those needs role "owner"), and its wallet is stuck.
func TestBughuntR19EraseLeavesOrgWithoutOwner(t *testing.T) {
	env := servertest.New(t)
	owner, ownerKey := env.NewUser("r19owner", false)
	_, memberKey := env.NewUser("r19member", false)
	if r := env.Do(ownerKey, "POST", "/api/v1/orgs", map[string]any{"name": "R19Org"}); r.Status/100 != 2 {
		t.Fatalf("create org: %s", r)
	}
	code := env.Do(ownerKey, "POST", "/api/v1/orgs/r19org/invites", map[string]any{"email": "m@example.test"}).Str("code")
	if r := env.Do(memberKey, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code}); r.Status != 200 {
		t.Fatalf("accept: %s", r)
	}
	browser, err := env.S.NewSession(owner.ID, "browser", "test")
	if err != nil {
		t.Fatal(err)
	}
	r := env.Do(browser, "POST", "/api/v1/account/erase", map[string]any{"confirmation": "erase my account"})
	var owners, members int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM org_members WHERE org_id=(SELECT id FROM orgs WHERE name='R19Org') AND role='owner'`).Scan(&owners)
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM org_members WHERE org_id=(SELECT id FROM orgs WHERE name='R19Org')`).Scan(&members)
	if r.Status == 200 && owners == 0 && members > 0 {
		t.Fatalf("the owner's erasure left an organization with %d member(s) and no owner", members)
	}
}

// The monthly quota (acct_billing.go grantQuotas) expires "the unused part of
// last month's quota" as min(balance, quotaGranted), ignoring how much of the
// quota the wallet used. A wallet that spent its whole quota but holds other
// credits (an admin grant, node earnings, a transfer) loses up to a full
// quota of THOSE credits at every month change.
func TestBughuntR19QuotaExpiryEatsOtherCreditsAfterQuotaWasSpent(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("r19quota", false)
	base := env.S.Balance(u.ID) // a new account starts with some credits
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/r19quota", map[string]any{"monthlyCredits": 10}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	env.S.BillNow() // grants this month's 10
	if b := env.S.Balance(u.ID); b != base+10 {
		t.Fatalf("setup: quota grant gave %v on top of %v", b, base)
	}
	// The wallet spends the whole quota, then an admin grants 100 more.
	if err := env.S.GrantCredits(u.ID, -10, "usage", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.S.GrantCredits(u.ID, 100, "top-up", ""); err != nil {
		t.Fatal(err)
	}
	// A month later.
	if _, err := env.S.DB.Exec(`UPDATE wallet_state SET quota_month='2000-01' WHERE wallet_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	env.S.BillNow()
	if b, want := env.S.Balance(u.ID), base+110; b != want {
		t.Fatalf("balance after the month change is %v, want %v (everything held plus the new 10): the expiry took %v credits that were not unused quota", b, want, want-b)
	}
}

// BILL-022: the admin's allowedSizes policy applies to create, fork (with the
// inherited size, bugs-r10-sonnet 4) and a create from a saved snapshot. A
// resume that names no type also starts the sandbox at its own size (here
// large), but policyGuard only looks at req["type"] for it: a wallet limited to
// small and default keeps starting large sandboxes with a plain resume.
func TestBughuntR19ResumeIgnoresAllowedSizesPolicy(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("r19size", false)
	_ = u
	id := createSandbox(t, env, key, map[string]any{"type": "large"})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/r19size", map[string]any{"allowedSizes": []string{"small", "default"}}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"}); r.Status != 403 {
		t.Fatalf("control: create at large must be refused: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status == 202 || r.Status == 200 {
		t.Fatalf("resume of a large sandbox answered %d although the wallet's policy allows only small and default", r.Status)
	}
}
