//go:build linux

package qemu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/node/vm/qemu/iso"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Real-VM tests. Run with:
//
//	CROWDVM_E2E=1 go test -v -run E2E -timeout 30m ./internal/node/vm/qemu/
//
// CROWDVM_E2E_GUEST=fake uses the stand-in guest (testdata/fakeguest)
// instead of building cmd/crowdvm-guest. CROWDVM_E2E_IMAGE=<qcow2> boots that
// base image instead of the test image. The test image (cloud image + the
// crowdvm-guest.service bootstrap unit + user "user", cloud-init disabled)
// is built once and cached under ~/.local/share/crowdvm-dev/vmdriver/testimage.

func devDir(t *testing.T) string {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".local", "share", "crowdvm-dev", "vmdriver")
}

func repoRoot(t *testing.T) string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

var guestOnce, cliOnce sync.Once
var guestBin, guestKind, cliBin string

// buildCLI builds the static linux/amd64 crowdvm CLI for the config drive.
func buildCLI(t *testing.T) string {
	cliOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "crowdvm-cli-bin")
		out := filepath.Join(dir, "crowdvm")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/crowdvm")
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Logf("build cli: %v\n%s", err, b)
			return
		}
		cliBin = out
	})
	return cliBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, b := range []string{guestBin, cliBin} {
		if b != "" {
			os.RemoveAll(filepath.Dir(b))
		}
	}
	os.Exit(code)
}

// buildGuest builds a static linux/amd64 guest agent: cmd/crowdvm-guest, or
// the stand-in when CROWDVM_E2E_GUEST=fake or the real one does not build.
func buildGuest(t *testing.T) (path, kind string) {
	guestOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "crowdvm-guest-bin")
		guestBin = filepath.Join(dir, "crowdvm-guest")
		build := func(pkg string) error {
			cmd := exec.Command("go", "build", "-o", guestBin, pkg)
			cmd.Dir = repoRoot(t)
			cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Logf("build %s: %v\n%s", pkg, err, out)
			}
			return err
		}
		if os.Getenv("CROWDVM_E2E_GUEST") != "fake" && build("./cmd/crowdvm-guest") == nil {
			guestKind = "real"
		} else if build("./internal/node/vm/qemu/testdata/fakeguest") == nil {
			guestKind = "fake"
		}
	})
	if guestKind == "" {
		t.Fatal("no guest binary")
	}
	return guestBin, guestKind
}

// testUserData turns the Ubuntu cloud image into a minimal CrowdVM base
// image: user "user", the bootstrap unit from ARCHITECTURE.md, DHCP on en*,
// cloud-init and snapd off, then power off.
const testUserData = `#cloud-config
users:
  - name: user
    uid: "1000"
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    lock_passwd: true
ssh_pwauth: false
write_files:
  - path: /etc/systemd/system/crowdvm-guest.service
    content: |
      [Unit]
      Description=CrowdVM guest agent
      Wants=dev-disk-by\x2dlabel-CROWDVM.device
      After=dev-disk-by\x2dlabel-CROWDVM.device
      [Service]
      ExecStartPre=/bin/mkdir -p /run/crowdvm/cfg
      ExecStartPre=/bin/sh -c 'mountpoint -q /run/crowdvm/cfg || mount -o ro LABEL=CROWDVM /run/crowdvm/cfg'
      ExecStartPre=/usr/bin/install -m 0755 /run/crowdvm/cfg/crowdvm-guest /usr/local/bin/crowdvm-guest
      ExecStart=/usr/local/bin/crowdvm-guest --config /run/crowdvm/cfg/config.json
      Restart=always
      RestartSec=1
      [Install]
      WantedBy=multi-user.target
  - path: /etc/netplan/01-crowdvm.yaml
    permissions: "0600"
    content: |
      network:
        version: 2
        ethernets:
          en:
            match: {name: "en*"}
            dhcp4: true
runcmd:
  - [rm, -f, /etc/netplan/50-cloud-init.yaml]
  - [systemctl, enable, crowdvm-guest.service]
  - [systemctl, mask, snapd.service, snapd.socket, snapd.seeded.service, snapd.apparmor.service]
  - [touch, /etc/cloud/cloud-init.disabled]
  - [fstrim, -av]
power_state:
  mode: poweroff
  delay: now
`

func cloudImage(t *testing.T) string {
	if p := os.Getenv("CROWDVM_CLOUD_IMAGE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "crowdvm", "images", "noble-server-cloudimg-amd64.img")
}

// testImage returns CROWDVM_E2E_IMAGE (e.g. an image from internal/node/image)
// or the cached test base image, building it on first use.
func testImage(t *testing.T) string {
	if p := os.Getenv("CROWDVM_E2E_IMAGE"); p != "" {
		return p
	}
	cloud := cloudImage(t)
	if _, err := os.Stat(cloud); err != nil {
		t.Skipf("cloud image: %v", err)
	}
	sum := sha256.Sum256([]byte(testUserData + cloud))
	out := filepath.Join(devDir(t), "testimage", fmt.Sprintf("base-%x.qcow2", sum[:6]))
	if _, err := os.Stat(out); err == nil {
		return out
	}
	os.MkdirAll(filepath.Dir(out), 0o755)
	work, err := os.MkdirTemp(filepath.Dir(out), "build-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	t0 := time.Now()
	disk := filepath.Join(work, "disk.qcow2")
	qimg(t, "create", "-q", "-f", "qcow2", "-b", cloud, "-F", "qcow2", disk)
	seed := filepath.Join(work, "seed.iso")
	if err := iso.WriteFile(seed, "cidata", []iso.File{
		{Name: "user-data", Data: []byte(testUserData)},
		{Name: "meta-data", Data: []byte("instance-id: crowdvm-test-image\nlocal-hostname: crowdvm\n")},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", "-nodefaults", "-display", "none",
		"-machine", "q35", "-accel", "kvm", "-cpu", "host", "-smp", "2", "-m", "2048",
		"-drive", "file="+disk+",if=virtio,format=qcow2,discard=unmap",
		"-drive", "file="+seed+",if=virtio,format=raw,readonly=on",
		"-netdev", "user,id=n0", "-device", "virtio-net-pci,netdev=n0",
		"-serial", "file:"+filepath.Join(work, "console.log"))
	if out, err := cmd.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(work, "console.log"))
		t.Fatalf("image build VM: %v %s\n%s", err, out, tailStr(string(log), 3000))
	}
	qimg(t, "convert", "-O", "qcow2", disk, out+".tmp")
	if err := os.Rename(out+".tmp", out); err != nil {
		t.Fatal(err)
	}
	t.Logf("built test image %s in %s", out, time.Since(t0).Round(time.Second))
	return out
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// copyFile clones (reflink when the filesystem can) or copies src to dst.
func copyFile(t *testing.T, src, dst string) {
	if out, err := exec.Command("cp", "--reflink=auto", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
}

type e2e struct {
	t      *testing.T
	d      *Driver
	opt    Options
	signer ssh.Signer
	pub    string
	timing []string
}

func (e *e2e) spec(id, parent string, boot proto.BootKind) proto.VMSpec {
	return proto.VMSpec{
		SandboxID: id, ImageID: "test-img", ParentID: parent, Nested: true,
		Size: proto.Size{Name: "test", VCPU: 2, MemoryMiB: 2048, DiskGiB: 8},
		Guest: proto.GuestConfig{SandboxID: id, Token: "tok-" + id, Hostname: id, Boot: boot,
			FirstBoot: boot == proto.BootCreate, SSHAuthorizedKeys: []string{e.pub}},
	}
}

func (e *e2e) timed(name string, f func()) {
	t0 := time.Now()
	f()
	line := fmt.Sprintf("%-34s %s", name, time.Since(t0).Round(10*time.Millisecond))
	e.timing = append(e.timing, line)
	e.t.Log("TIMING " + line)
}

func (e *e2e) start(spec proto.VMSpec) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := e.d.Start(ctx, spec); err != nil {
		e.t.Fatalf("start %s: %v", spec.SandboxID, err)
	}
	e.wait(spec.SandboxID)
}

func (e *e2e) wait(id string) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := e.d.Wait(ctx, id); err != nil {
		log, _ := os.ReadFile(filepath.Join(e.opt.DataDir, "vms", id, "console.log"))
		e.t.Fatalf("wait %s: %v\nconsole:\n%s", id, err, tailStr(string(log), 4000))
	}
}

// sh runs a command as user over SSH, tunnelled through the guest agent to
// sshd on guest port 22.
func (e *e2e) sh(id, cmd string) string {
	e.t.Helper()
	var last error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := e.d.DialGuest(ctx, id, 22)
		cancel()
		if err != nil {
			last = err
			continue
		}
		conn.SetDeadline(time.Now().Add(60 * time.Second))
		c, chans, reqs, err := ssh.NewClientConn(conn, "127.0.0.1:22", &ssh.ClientConfig{
			User: proto.GuestUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(e.signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second,
		})
		if err != nil {
			conn.Close()
			last = err
			continue
		}
		client := ssh.NewClient(c, chans, reqs)
		s, err := client.NewSession()
		if err != nil {
			client.Close()
			last = err
			continue
		}
		out, err := s.CombinedOutput(cmd)
		client.Close()
		if err != nil {
			e.t.Fatalf("%s: %q: %v: %s", id, cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	e.t.Fatalf("%s: ssh %q: %v", id, cmd, last)
	return ""
}

func (e *e2e) waitState(id, state string) proto.VMStatus {
	e.t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st, _ := e.d.Status(id); st.State == state {
			return st
		}
	}
	st, _ := e.d.Status(id)
	e.t.Fatalf("%s never reached %s: %+v", id, state, st)
	return st
}

// logSize reports a snapshot's file size and how much of it is allocated.
func (e *e2e) logSize(ref proto.SnapshotRef) {
	p, _ := e.d.Store().SnapshotPath(ref.ID)
	var st syscall.Stat_t
	syscall.Stat(p, &st)
	e.t.Logf("snapshot %s (parent %q): file %d MiB, allocated %d MiB", ref.ID, ref.ParentID, ref.SizeBytes>>20, st.Blocks*512>>20)
}

func (e *e2e) want(id, cmd, want string) {
	e.t.Helper()
	if got := e.sh(id, cmd); got != want {
		e.t.Fatalf("%s: %q = %q, want %q", id, cmd, got, want)
	}
}

func TestE2E(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1 to boot real VMs")
	}
	if runtime.GOOS != "linux" || defaultAccel() != "kvm" {
		t.Skip("needs Linux with /dev/kvm")
	}
	needQemuIO(t)
	bin, kind := buildGuest(t)
	base := testImage(t)
	t.Logf("guest agent: %s", kind)

	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-%d", time.Now().UnixNano()))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
	e.opt = Options{DataDir: dataDir, GuestBinary: bin, CLIBinary: buildCLI(t), MaxChain: 3, BootTimeout: 2 * time.Minute,
		Net: os.Getenv("CROWDVM_E2E_NET"), Logf: t.Logf} // CROWDVM_E2E_NET=slirp runs the suite on the fallback
	var err error
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range e.d.List() {
			e.d.Destroy(context.Background(), s.SandboxID)
		}
		e.d.Close()
		if out, _ := exec.Command("pgrep", "-f", dataDir).Output(); len(out) > 0 {
			t.Errorf("qemu processes left: %s", out)
		}
		os.RemoveAll(dataDir)
		t.Log("timings:\n  " + strings.Join(e.timing, "\n  "))
	})
	ctx := context.Background()
	imp := filepath.Join(dataDir, "import.qcow2")
	copyFile(t, base, imp)
	if err := e.d.Store().ImportImage("test-img", imp); err != nil {
		t.Fatal(err)
	}

	// Create + boot; exec through the tunnel to sshd; nested virt.
	e.timed("create: start -> guest healthy", func() { e.start(e.spec("a", "", proto.BootCreate)) })
	e.want("a", "echo one > one.txt && cat one.txt", "one")
	e.want("a", "test -e /dev/kvm && echo kvm", "kvm")
	if e.opt.CLIBinary != "" { // in-sandbox CLI on the config drive (TOOL-007)
		e.want("a", "test -x /run/crowdvm/cfg/crowdvm && echo cli", "cli")
	}
	if kind == "real" {
		e.want("a", "hostname", "a")
		// SIZE-006: the 8 GiB are for user data; the OS comes on top.
		if gb, _ := strconv.Atoi(e.sh("a", "for i in $(seq 30); do a=$(df --output=avail -BG / | tail -1 | tr -d ' G'); [ $a -ge 7 ] && break; sleep 1; done; echo $a")); gb < 7 {
			t.Fatalf("root fs must offer ~8 GiB free on an 8 GiB disk: %d GiB", gb)
		}
		if e.opt.CLIBinary != "" {
			e.sh("a", "for i in $(seq 20); do test -x /usr/local/bin/crowdvm && break; sleep 0.5; done")
			e.want("a", "readlink /usr/local/bin/host", "crowdvm")
		}
	}

	// Live snapshot + fork running alongside the source.
	var s1 proto.SnapshotRef
	e.timed("live snapshot (freeze/thaw)", func() { s1, err = e.d.Snapshot(ctx, "a", "s1") })
	if err != nil || s1.ParentID != "" || s1.ImageID != "test-img" {
		t.Fatalf("snapshot s1: %+v %v", s1, err)
	}
	e.want("a", "touch thawed && echo ok", "ok") // filesystems thawed
	// Change tracking for periodic snapshots: writes are seen.
	e.sh("a", "echo w > w.txt && sync")
	if ch, err := e.d.DiskChanged("a"); err != nil || !ch {
		t.Fatalf("DiskChanged after a write: %v %v", ch, err)
	}
	e.timed("fork: start -> guest healthy", func() { e.start(e.spec("b", "s1", proto.BootFork)) })
	e.want("b", "cat one.txt", "one")
	e.sh("a", "echo A > x.txt")
	e.sh("b", "echo B > x.txt")
	e.want("a", "cat x.txt", "A")
	e.want("b", "cat x.txt", "B")

	// Guest reboot: starting -> running again. Guest poweroff: error.
	e.sh("b", "sudo systemd-run --on-active=1 --timer-property=AccuracySec=100ms systemctl reboot")
	e.timed("guest reboot -> guest healthy", func() {
		e.waitState("b", proto.VMStarting)
		e.wait("b")
	})
	e.want("b", "cat x.txt", "B")
	e.sh("b", "sudo systemd-run --on-active=1 --timer-property=AccuracySec=100ms systemctl poweroff")
	if st := e.waitState("b", proto.VMError); st.Error != "guest powered off" {
		t.Fatalf("poweroff: %+v", st)
	}
	if err := e.d.Destroy(ctx, "b"); err != nil {
		t.Fatal(err)
	}

	// Stop -> snapshot; resume; the file written before the stop survives.
	// Boat stop: the pre-stop hook runs; the OS is not shut down, so a
	// service's ExecStop never runs (SNAP-005/006).
	if kind == "real" {
		e.sh("a", "mkdir -p .crowdvm && printf '#!/bin/sh\\necho hook-$(id -un) > $HOME/hook.txt\\n' > .crowdvm/pre-stop && chmod +x .crowdvm/pre-stop")
		e.sh("a", `sudo systemd-run --unit=stopmark -p ExecStop='/bin/sh -c "echo shutdown > /home/user/shutdown.txt; sync"' /bin/sleep infinity`)
	}
	var s2 proto.SnapshotRef
	e.timed("stop (prepare-stop + freeze + QMP quit)", func() { s2, err = e.d.Stop(ctx, "a", "s2", false) })
	if err != nil || s2.ParentID != "s1" {
		t.Fatalf("stop: %+v %v", s2, err)
	}
	e.logSize(s2)
	e.timed("resume: start -> guest healthy", func() { e.start(e.spec("a", "s2", proto.BootResume)) })
	e.want("a", "cat one.txt x.txt", "one\nA")
	if kind == "real" {
		e.want("a", "cat hook.txt; test -e shutdown.txt && echo 'OS was shut down'; true", "hook-user")
	}

	// Live snapshots past MaxChain=3: s4 is flattened by block-stream.
	e.sh("a", "echo after > after.txt && sync")
	if s3, err := e.d.Snapshot(ctx, "a", "s3"); err != nil || s3.ParentID != "s2" {
		t.Fatalf("s3: %+v %v", s3, err)
	}
	var s4 proto.SnapshotRef
	e.timed("live snapshot with merge", func() { s4, err = e.d.Snapshot(ctx, "a", "s4") })
	if err != nil || s4.ParentID != "" || strings.Join(s4.Supersedes, ",") != "s3,s2,s1" {
		t.Fatalf("s4 must absorb s3..s1: %+v %v", s4, err)
	}
	e.logSize(s4)
	e.want("a", "cat one.txt after.txt", "one\nafter")

	// A hung guest (vCPUs paused) stops answering health probes; recover
	// power-cycles it (LIFE-023/027).
	e.d.mu.Lock()
	hr, hm := e.d.vms["a"].run, e.d.vms["a"]
	e.d.mu.Unlock()
	if err := hr.q.cmd(ctx, "stop", nil, nil); err != nil {
		t.Fatal(err)
	}
	if e.d.guest(hm).HealthyWithin(ctx, 3*time.Second) {
		t.Fatal("paused guest still answers")
	}
	// Recover: same overlay, hard restart, boot kind "recover".
	e.timed("recover hung guest: restart -> guest healthy", func() {
		if err := e.d.Restart(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		e.wait("a")
	})
	e.want("a", "cat after.txt", "after")
	e.want("a", "sudo grep -o '\"boot\":\"recover\"' /run/crowdvm/cfg/config.json", `"boot":"recover"`)

	// Node restart: a new driver adopts the running VM.
	e.d.Close()
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	e.wait("a")
	e.want("a", "cat after.txt", "after")

	// Node restart after qemu died too: error state, overlay kept, Stop saves it.
	e.sh("a", "echo crash > crash.txt && sync")
	e.d.mu.Lock()
	pid := e.d.vms["a"].rec.PID
	e.d.mu.Unlock()
	e.d.Close()
	syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.d.Status("a"); st.State != proto.VMError {
		t.Fatalf("dead VM after node restart: %+v", st)
	}
	s5, err := e.d.Stop(ctx, "a", "s5", false)
	if err != nil || s5.ParentID != "s4" {
		t.Fatalf("stop dead VM: %+v %v", s5, err)
	}
	e.start(e.spec("a", "s5", proto.BootResume))
	e.want("a", "cat crash.txt one.txt", "crash\none")

	// Destroy; snapshots deleted -> only the image is left.
	if err := e.d.Destroy(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		e.d.Store().DeleteSnapshot(id)
	}
	if ents, _ := os.ReadDir(filepath.Join(dataDir, "disks")); len(ents) != 1 {
		t.Fatalf("disks left after cleanup: %v", ents)
	}
}

// TestE2EBootProfile prints where boot time goes (CROWDVM_E2E=1 and
// CROWDVM_E2E_PROFILE=1; CROWDVM_E2E_PROFILE_CMD adds a guest command).
func TestE2EBootProfile(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" || os.Getenv("CROWDVM_E2E_PROFILE") != "1" {
		t.Skip("set CROWDVM_E2E=1 CROWDVM_E2E_PROFILE=1")
	}
	bin, _ := buildGuest(t)
	base := testImage(t)
	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-%d", time.Now().UnixNano()))
	defer os.RemoveAll(dataDir)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
	e.opt = Options{DataDir: dataDir, GuestBinary: bin, Logf: t.Logf}
	var err error
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	defer e.d.Close()
	defer e.d.Destroy(context.Background(), "p")
	imp := filepath.Join(dataDir, "import.qcow2")
	copyFile(t, base, imp)
	if err := e.d.Store().ImportImage("test-img", imp); err != nil {
		t.Fatal(err)
	}
	e.timed("boot", func() { e.start(e.spec("p", "", proto.BootCreate)) })
	t.Log("\n" + e.sh("p", "systemd-analyze; systemd-analyze critical-chain crowdvm-guest.service; systemd-analyze blame | head -15; cat /proc/cmdline; "+os.Getenv("CROWDVM_E2E_PROFILE_CMD")+""))
	log, _ := os.ReadFile(filepath.Join(dataDir, "vms", "p", "console.log"))
	t.Log("\nconsole head:\n" + string(log[:min(len(log), 3000)]))
}

// TestE2ESnapshotCost measures 60 s periodic snapshots of an idle VM and of
// one with a small steady write load: layer sizes, how often the change
// tracker lets an idle minute skip, and the chain depth with compaction
// (CROWDVM_E2E=1 CROWDVM_E2E_SNAPCOST=<minutes>).
func TestE2ESnapshotCost(t *testing.T) {
	mins, _ := strconv.Atoi(os.Getenv("CROWDVM_E2E_SNAPCOST"))
	if os.Getenv("CROWDVM_E2E") != "1" || mins <= 0 {
		t.Skip("set CROWDVM_E2E=1 CROWDVM_E2E_SNAPCOST=<minutes>")
	}
	bin, _ := buildGuest(t)
	base := testImage(t)
	dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2e-%d", time.Now().UnixNano()))
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
	e.opt = Options{DataDir: dataDir, GuestBinary: bin, Logf: func(string, ...any) {}}
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
	imp := filepath.Join(dataDir, "import.qcow2")
	copyFile(t, base, imp)
	if err := e.d.Store().ImportImage("test-img", imp); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{"idle", "busy"} {
		e.start(e.spec(id, "", proto.BootCreate))
	}
	// busy: ~1 MiB of new log data per minute, like a dev server.
	e.sh("busy", "nohup sh -c 'while :; do head -c 17000 /dev/urandom | base64 >> app.log; sleep 1; done' >/dev/null 2>&1 &")
	time.Sleep(30 * time.Second) // boot settles
	for _, id := range []string{"idle", "busy"} {
		if _, err := e.d.Snapshot(ctx, id, id+"-0"); err != nil {
			t.Fatal(err)
		}
	}
	type acc struct{ snaps, skips, bytes, maxDepth int64 }
	res := map[string]*acc{"idle": {}, "busy": {}}
	for i := 1; i <= mins; i++ {
		time.Sleep(time.Minute)
		for _, id := range []string{"idle", "busy"} {
			a := res[id]
			e.d.guest(e.d.vms[id]).Post(ctx, "/v1/internal/sync", nil, time.Minute)
			if ch, _ := e.d.DiskChanged(id); !ch {
				a.skips++
				continue
			}
			ref, err := e.d.Snapshot(ctx, id, fmt.Sprintf("%s-%d", id, i))
			if err != nil {
				t.Fatal(err)
			}
			a.snaps++
			a.bytes += ref.SizeBytes
			e.d.mu.Lock()
			depth := int64(len(e.d.store.chain(e.d.vms[id].rec.Parent)))
			e.d.mu.Unlock()
			a.maxDepth = max(a.maxDepth, depth)
			t.Logf("%s minute %d: layer %d KiB, merged %d, chain depth %d", id, i, ref.SizeBytes>>10, len(ref.Supersedes), depth)
		}
	}
	for id, a := range res {
		t.Logf("RESULT %s: %d minutes, %d snapshots (%d skipped as unchanged), %.1f MiB uploaded (%.0f KiB/min), max chain depth %d",
			id, mins, a.snaps, a.skips, float64(a.bytes)/(1<<20), float64(a.bytes)/1024/float64(mins), a.maxDepth)
	}
}
