//go:build linux

package qemu

import (
	"bufio"
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

// TestE2ELeftovers boots the real guest on a full image and checks the
// guest/node rows: LIFE-009 (ready after the repo clones), SIZE-014/016
// (/dev/kvm usable by default), KEY-017/ENV-016/AIC-011 (credential files,
// /login guard), URL-012 (direct TCP/UDP to the guest NIC through ufw) and
// LIFE-018 (startup playbook on resume).
//
//	CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v2.qcow2 \
//	  go test -v -run E2ELeftovers -timeout 30m ./internal/node/vm/qemu/
func TestE2ELeftovers(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" || os.Getenv("CROWDVM_E2E_IMAGE") == "" {
		t.Skip("set CROWDVM_E2E=1 and CROWDVM_E2E_IMAGE=<full image>")
	}
	if defaultAccel() != "kvm" {
		t.Skip("needs /dev/kvm")
	}
	bin, kind := buildGuest(t)
	if kind != "real" {
		t.Skip("needs the real guest agent")
	}
	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-lo-%d", time.Now().UnixNano()))
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
	sp := e.spec("lo", "", proto.BootCreate)
	sp.Nested = false // SIZE-016: /dev/kvm without asking for it
	sp.Size = proto.Size{Name: "test", VCPU: 2, MemoryMiB: 4096, DiskGiB: 8}
	sp.Guest.Env = map[string]string{"CROWDVM_API_KEY": "cvm_e2e_key", "CROWDVM_API_URL": "http://10.0.2.2:8420/api/v1",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-e2e"}
	sp.Guest.Repos = []proto.Repo{{URL: "https://github.com/octocat/Hello-World.git", Path: "hello"}}
	e.start(sp)

	// LIFE-009: the guest says reposCloning until the clone is on disk.
	health := func() map[string]any {
		addr, _ := e.d.GuestAddr("lo")
		resp, err := http.Get("http://" + addr + "/v1/health")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	t0 := time.Now()
	for h := health(); h == nil || h["reposCloning"] != false; h = health() {
		if time.Since(t0) > 3*time.Minute {
			t.Fatalf("still cloning: %v", h)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("TIMING clones done %s after guest healthy", time.Since(t0).Round(10*time.Millisecond))
	e.want("lo", "test -d hello/.git && echo cloned", "cloned")

	// SIZE-014/016: KVM usable by the user (an Android emulator's need).
	e.want("lo", `python3 -c "import fcntl,os; f=os.open('/dev/kvm',os.O_RDWR); print(fcntl.ioctl(f,0xAE00), fcntl.ioctl(f,0xAE01,0)>0)"`, "12 True")

	// KEY-017 / ENV-016 / AIC-011.
	e.want("lo", `jq -r .profiles.default.apiKey,.profiles.default.server ~/.config/crowdvm/config.json`, "cvm_e2e_key\nhttp://10.0.2.2:8420")
	e.want("lo", `jq -r .profiles.default.apiKey ~/.config/ascii/boat/config.json; stat -c %a ~/.config/crowdvm/config.json`, "cvm_e2e_key\n600")
	e.want("lo", `jq -r .claudeAiOauth.accessToken ~/.claude/.credentials.json`, "sk-ant-oat01-e2e")
	e.want("lo", `bash -ic 'echo "[$CLAUDE_CODE_OAUTH_TOKEN]"' 2>/dev/null | tail -1`, "[sk-ant-oat01-e2e]")
	e.sh("lo", `echo '{"claudeAiOauth":{"accessToken":"u","refreshToken":"r","expiresAt":1}}' > ~/.claude/.credentials.json`)
	e.want("lo", `bash -ic 'echo "[$CLAUDE_CODE_OAUTH_TOKEN]"' 2>/dev/null | tail -1; bash -lc 'echo "[$CLAUDE_CODE_OAUTH_TOKEN]"'`, "[]\n[]")

	// URL-012: the node reaches the guest NIC; ufw decides.
	e.sh("lo", "setsid nohup python3 -m http.server 8000 >/dev/null 2>&1 < /dev/null & "+
		`setsid nohup python3 -c "import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(('0.0.0.0',9999))
while True:
  d,a=s.recvfrom(2048); s.sendto(d,a)" >/dev/null 2>&1 < /dev/null & sleep 1; echo started`)
	dial := func(network string, port int) error {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c, err := e.d.DialGuestNIC(dctx, "lo", network, port)
		if err != nil {
			return err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if network == "udp" {
			c.Write([]byte("ping"))
			b := make([]byte, 16)
			n, err := c.Read(b)
			if err != nil || string(b[:n]) != "ping" {
				return fmt.Errorf("udp echo %q %v", b[:n], err)
			}
			return nil
		}
		fmt.Fprintf(c, "GET / HTTP/1.0\r\n\r\n")
		line, err := bufio.NewReader(c).ReadString('\n')
		if !strings.Contains(line, "200") {
			return fmt.Errorf("http: %q %v", line, err)
		}
		return nil
	}
	if err := dial("tcp", 8000); err == nil {
		t.Fatal("ufw default deny did not apply to the direct port")
	}
	e.sh("lo", "sudo ufw allow 8000/tcp >/dev/null && sudo ufw allow 9999/udp >/dev/null && echo ok")
	if err := dial("tcp", 8000); err != nil {
		t.Fatalf("direct tcp after ufw allow: %v", err)
	}
	if err := dial("udp", 9999); err != nil {
		t.Fatalf("direct udp after ufw allow: %v", err)
	}

	// LIFE-018: stop, resume; files opened after the start land in the playbook.
	e.sh("lo", "echo data > ~/app.txt && sync")
	if _, err := e.d.Stop(ctx, "lo", "s1", false); err != nil {
		t.Fatal(err)
	}
	rs := e.spec("lo", "s1", proto.BootResume)
	rs.Nested = false
	e.start(rs)
	e.sh("lo", "cat ~/app.txt hello/README >/dev/null && echo read")
	e.d.mu.Lock()
	m := e.d.vms["lo"]
	e.d.mu.Unlock()
	if err := e.d.guest(m).Post(ctx, "/v1/internal/playbook/finish", nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	out := e.sh("lo", "jq -r '.paths[]' ~/.crowdvm/playbook.json")
	for _, want := range []string{"/home/user/app.txt", "/home/user/hello/README"} {
		if !strings.Contains(out, want+"\n") && !strings.HasSuffix(out, want) {
			t.Fatalf("playbook lacks %s (%d paths):\n%s", want, strings.Count(out, "\n")+1, tailStr(out, 2000))
		}
	}
	t.Logf("playbook: %d paths, first: %q", strings.Count(out, "\n")+1, strings.SplitN(out, "\n", 6)[:5])
}
