//go:build linux

package qemu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/crowdvm/crowdvm/internal/proto"
)

const internetFile = "https://proof.ovh.net/files/100Mb.dat"

// TestE2ENet checks guest networking on real VMs: with netstack the guest
// reaches the internet and the server (also as 10.0.2.2:<port>) but not this
// machine's loopback, LAN or tailnet addresses; then it measures throughput
// and latency against slirp. Run with:
//
//	CROWDVM_E2E=1 go test -v -run E2ENet -timeout 20m ./internal/node/vm/qemu/
//
// Needs internet access from this machine.
func TestE2ENet(t *testing.T) {
	if os.Getenv("CROWDVM_E2E") != "1" {
		t.Skip("set CROWDVM_E2E=1 to boot real VMs")
	}
	if runtime.GOOS != "linux" || defaultAccel() != "kvm" {
		t.Skip("needs Linux with /dev/kvm")
	}
	bin, _ := buildGuest(t)
	base := testImage(t)

	// The "CrowdVM server" (allowed) and services that must stay unreachable.
	srvL, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(srvL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // uploads
		mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
		if mb == 0 {
			io.WriteString(w, "crowdvm-server-ok")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(mb<<20))
		buf := make([]byte, 1<<20)
		for range mb {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer srvL.Close()
	srvPort := srvL.Addr().(*net.TCPAddr).Port
	leak := func(addr string) (int, *atomic.Int32) {
		var hits atomic.Int32
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				hits.Add(1)
				c.Write([]byte("LEAK"))
				c.Close()
			}
		}()
		return l.Addr().(*net.TCPAddr).Port, &hits
	}
	lanPort, lanHits := leak("0.0.0.0:0") // every address of this machine
	loPort, loHits := leak("127.0.0.1:0") // like the node UI on 8421
	udpL, _ := net.ListenPacket("udp", "0.0.0.0:0")
	defer udpL.Close()
	var udpHits atomic.Int32
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := udpL.ReadFrom(buf); err != nil {
				return
			}
			udpHits.Add(1)
		}
	}()
	udpPort := udpL.LocalAddr().(*net.UDPAddr).Port
	var hostIPs []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && !ip.IsLoopback() {
				hostIPs = append(hostIPs, ip.Unmap().String())
			}
		}
	}
	t.Logf("host addresses probed: %v", hostIPs)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	results := map[string]map[string]string{}

	boot := func(mode string) *e2e {
		dataDir := filepath.Join(devDir(t), fmt.Sprintf("e2enet-%s-%d", mode, time.Now().UnixNano()))
		e := &e2e{t: t, signer: signer, pub: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))}
		e.opt = Options{DataDir: dataDir, GuestBinary: bin, Net: mode, ServerAddr: srvL.Addr().String(),
			BootTimeout: 2 * time.Minute, Logf: t.Logf}
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
		return e
	}
	// probe returns what a TCP connect from the guest read ("" = refused/timeout).
	probe := func(e *e2e, id, ip string, port int) string {
		return e.sh(id, fmt.Sprintf(`timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%d && head -c4 <&3' 2>/dev/null; true`, ip, port))
	}
	measure := func(e *e2e, id, mode string) {
		r := map[string]string{}
		results[mode] = r
		bps, _ := strconv.ParseFloat(e.sh(id, fmt.Sprintf(`curl -sS -o /dev/null -w '%%{speed_download}' 'http://10.0.2.2:%d/?mb=2048'`, srvPort)), 64)
		r["node-local download (2 GiB)"] = fmt.Sprintf("%.0f Mbit/s", bps*8/1e6)
		if mode == NetNetstack && bps*8/1e6 < 300 {
			t.Errorf("netstack download %.0f Mbit/s < 300", bps*8/1e6)
		}
		bps, _ = strconv.ParseFloat(e.sh(id, `curl -sS -m 60 -o /dev/null -w '%{speed_download}' `+internetFile+` 2>/dev/null || true`), 64)
		r["internet download (100 MiB, OVH)"] = fmt.Sprintf("%.0f Mbit/s", bps*8/1e6)
		r["upload to node (1 GiB)"] = func() string {
			bps, _ := strconv.ParseFloat(e.sh(id, fmt.Sprintf(`head -c 1073741824 /dev/zero | curl -sS -o /dev/null -w '%%{speed_upload}' -T - 'http://10.0.2.2:%d/'`, srvPort)), 64)
			return fmt.Sprintf("%.0f Mbit/s", bps*8/1e6)
		}()
		r["TCP connect to node (avg of 20)"] = e.sh(id, fmt.Sprintf(`for i in $(seq 20); do curl -sS -o /dev/null -w '%%{time_connect}\n' http://10.0.2.2:%d/; done | awk '{s+=$1} END {printf "%%.3f ms", s*1000/NR}'`, srvPort))
		r["HTTP request to node (avg of 20)"] = e.sh(id, fmt.Sprintf(`for i in $(seq 20); do curl -sS -o /dev/null -w '%%{time_total}\n' http://10.0.2.2:%d/; done | awk '{s+=$1} END {printf "%%.3f ms", s*1000/NR}'`, srvPort))
		r["ping 1.1.1.1 (avg of 5)"] = e.sh(id, `ping -q -c 5 -i 0.2 -W 3 1.1.1.1 2>&1 | awk -F/ '/rtt|round-trip/ {print $5 " ms"}'; true`)
		r["TCP connect 1.1.1.1:443 (avg of 10)"] = e.sh(id, `for i in $(seq 10); do curl -sS -o /dev/null -m 5 -w '%{time_connect}\n' https://1.1.1.1/ 2>/dev/null; done | awk '{s+=$1} END {if (NR) printf "%.2f ms", s*1000/NR}'`)
		for k, v := range r {
			t.Logf("PERF %-6s %-40s %s", mode, k, v)
		}
	}

	if out, err := exec.Command("curl", "-sS", "-m", "60", "-o", "/dev/null", "-w", "%{speed_download}", internetFile).Output(); err == nil {
		bps, _ := strconv.ParseFloat(string(out), 64)
		t.Logf("PERF host   internet download (100 MiB, OVH)         %.0f Mbit/s", bps*8/1e6)
	}

	// --- netstack (default) ---
	e := boot(NetNetstack)
	e.start(e.spec("a", "", proto.BootCreate))
	sb := e.spec("c", "", proto.BootCreate)
	sb.Net = &proto.NetPolicy{Deny: []string{"1.0.0.0/8"}}
	e.start(sb)

	e.want("a", "cat /sys/class/net/en*/mtu | head -1", "65520")
	e.want("a", "ip -4 -o addr show scope global | awk '$2 ~ /^en/ {print $4}'", "10.0.2.15/24")
	if out := e.sh("a", "getent hosts example.com | head -1"); out == "" {
		t.Fatal("DNS: example.com did not resolve")
	}
	e.want("a", "curl -sS -m 20 -o /dev/null -w '%{http_code}' https://example.com/", "200")
	e.want("a", fmt.Sprintf("curl -sS -m 5 http://10.0.2.2:%d/", srvPort), "crowdvm-server-ok")
	if out := e.sh("a", `python3 -c '
import socket; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(5)
s.sendto(bytes.fromhex("abcd01000001000000000000076578616d706c6503636f6d0000010001"), ("1.1.1.1", 53))
print(len(s.recv(512)) > 12)'`); out != "True" {
		t.Fatalf("UDP to 1.1.1.1:53: %q", out)
	}
	if out := e.sh("a", "ping -c 2 -W 3 1.1.1.1 >/dev/null && echo ok || echo fail"); out != "ok" {
		t.Errorf("ICMP echo to 1.1.1.1: %s", out)
	}
	// Containment: the host alias, this machine's addresses, the LAN.
	blocked := []struct {
		ip   string
		port int
	}{{"10.0.2.2", lanPort}, {"10.0.2.2", loPort}, {"10.0.2.2", 22}}
	for _, ip := range hostIPs {
		blocked = append(blocked, struct {
			ip   string
			port int
		}{ip, lanPort})
	}
	for _, b := range blocked {
		if got := probe(e, "a", b.ip, b.port); got != "" {
			t.Errorf("guest reached %s:%d: %q", b.ip, b.port, got)
		}
	}
	for _, ip := range hostIPs {
		e.sh("a", fmt.Sprintf(`python3 -c 'import socket; socket.socket(socket.AF_INET, socket.SOCK_DGRAM).sendto(b"x", ("%s", %d))'; true`, ip, udpPort))
	}
	if out := e.sh("a", "ping -c 1 -W 2 192.168.178.1 2>&1; true"); !strings.Contains(out, "100% packet loss") && !strings.Contains(out, "filtered") {
		t.Errorf("ping to the LAN: %s", out)
	}
	time.Sleep(time.Second)
	if lanHits.Load() != 0 || loHits.Load() != 0 || udpHits.Load() != 0 {
		t.Errorf("host services hit: tcp(any addr)=%d tcp(loopback)=%d udp=%d", lanHits.Load(), loHits.Load(), udpHits.Load())
	}
	// Per-sandbox policy narrows egress; the server stays reachable.
	if got := e.sh("c", "timeout 5 bash -c '</dev/tcp/1.1.1.1/443' 2>/dev/null && echo open || echo closed"); got != "closed" {
		t.Errorf("sandbox deny 1.0.0.0/8: 1.1.1.1:443 %s", got)
	}
	e.want("c", "curl -sS -m 20 -o /dev/null -w '%{http_code}' https://example.com/", "200")
	e.want("c", fmt.Sprintf("curl -sS -m 5 http://10.0.2.2:%d/", srvPort), "crowdvm-server-ok")
	e.d.Destroy(context.Background(), "c")

	// Node restart: the adopted VM gets a new network and still works.
	e.d.Close()
	var err error
	if e.d, err = New(e.opt); err != nil {
		t.Fatal(err)
	}
	e.wait("a")
	e.want("a", fmt.Sprintf("curl -sS -m 10 --retry 3 --retry-connrefused http://10.0.2.2:%d/", srvPort), "crowdvm-server-ok")
	e.want("a", "curl -sS -m 20 -o /dev/null -w '%{http_code}' https://example.com/", "200")
	measure(e, "a", "netstack")
	e.d.Destroy(context.Background(), "a")

	// --- slirp, for comparison (and to document what it leaks) ---
	if os.Getenv("CROWDVM_E2E_NET_SLIRP") == "0" {
		return
	}
	s := boot(NetSlirp)
	s.start(s.spec("s", "", proto.BootCreate))
	s.want("s", fmt.Sprintf("curl -sS -m 5 http://10.0.2.2:%d/", srvPort), "crowdvm-server-ok")
	t.Logf("slirp: guest -> host loopback via 10.0.2.2:%d read %q (netstack: blocked)", loPort, probe(s, "s", "10.0.2.2", loPort))
	measure(s, "s", "slirp")
	for k := range results["netstack"] {
		t.Logf("COMPARE %-40s netstack %-14s slirp %s", k, results["netstack"][k], results["slirp"][k])
	}
}
