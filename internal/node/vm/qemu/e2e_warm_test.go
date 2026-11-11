//go:build linux

package qemu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// TestE2EWarm (SIZE-012): a warm pool VM is adopted by a sandbox (guest
// POST /v1/internal/adopt + Rename) and then behaves like a fresh create:
// its identity, survives a guest reboot and stops/resumes under its id.
//
//	CROWDVM_E2E=1 [CROWDVM_E2E_IMAGE=...] go test -v -run E2EWarm -timeout 20m ./internal/node/vm/qemu/
func TestE2EWarm(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" || defaultAccel() != "kvm" {
		t.Skip("set CROWDVM_E2E=1 (needs /dev/kvm)")
	}
	bin, kind := buildGuest(t)
	if kind != "real" {
		t.Skip("needs the real guest agent")
	}
	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-warm-%d", time.Now().UnixNano()))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
	e.opt = Options{DataDir: dataDir, GuestBinary: bin, BootTimeout: 3 * time.Minute, Logf: t.Logf}
	var err error
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range e.d.List() {
			e.d.Destroy(context.Background(), s.SandboxID)
		}
		e.d.Close()
		os.RemoveAll(dataDir)
	})
	ctx := context.Background()
	imp := filepath.Join(dataDir, "import.qcow2")
	copyFile(t, testImage(t), imp)
	if err := e.d.Store().ImportImage("test-img", imp); err != nil {
		t.Fatal(err)
	}
	warm := e.spec("warm_x1", "", proto.BootWarm)
	warm.Guest = proto.GuestConfig{SandboxID: "warm_x1", Token: "warm-tok", Boot: proto.BootWarm, ListenAddr: ":7070"}
	e.start(warm)
	addr, _ := e.d.GuestAddr("warm_x1")
	health := func() map[string]any {
		resp, err := http.Get("http://" + addr + "/v1/health")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	if h := health(); h["warm"] != true {
		t.Fatalf("warm health: %v", h)
	}
	sp := e.spec("sbw", "", proto.BootCreate)
	t0 := time.Now()
	g := e.d.guest(e.d.vms["warm_x1"])
	if err := g.Post(ctx, "/v1/internal/adopt", sp.Guest, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	for h := health(); h == nil || h["sandboxId"] != "sbw"; h = health() {
		if time.Since(t0) > time.Minute {
			t.Fatalf("not adopted: %v", h)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := e.d.Rename(ctx, "warm_x1", sp); err != nil {
		t.Fatal(err)
	}
	t.Logf("TIMING warm adoption -> guest answers as the sandbox: %s", time.Since(t0).Round(10*time.Millisecond))
	if st, _ := e.d.Status("sbw"); st.State != proto.VMRunning || st.SandboxID != "sbw" {
		t.Fatalf("status after rename: %+v", st)
	}
	e.want("sbw", "hostname; sudo cat /etc/crowdvm/sandbox-id", "sbw\nsbw")
	// A guest reboot keeps the adopted identity (config drive still says warm).
	e.sh("sbw", "echo kept > k.txt && sync && sudo systemd-run --on-active=1 --timer-property=AccuracySec=100ms systemctl reboot")
	e.waitState("sbw", proto.VMStarting)
	e.wait("sbw")
	e.want("sbw", "hostname; cat k.txt", "sbw\nkept")
	s1, err := e.d.Stop(ctx, "sbw", "w1", false)
	if err != nil {
		t.Fatal(err)
	}
	e.start(e.spec("sbw", s1.ID, proto.BootResume))
	e.want("sbw", "cat k.txt", "kept")
}
