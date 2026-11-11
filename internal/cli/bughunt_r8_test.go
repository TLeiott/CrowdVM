package cli

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// Helper process for TestBughuntR8NohupIsIgnored: runs the real Main.
func TestBughuntR8SighupHelperProc(t *testing.T) {
	if os.Getenv("R8_SIGHUP_HELPER") != "1" {
		t.Skip("helper process")
	}
	f := clienttest.New(t)
	id := f.AddSandbox("ready")
	os.Setenv("CROWDVM_API_BASE", f.URL+"/api/v1")
	os.Setenv("CROWDVM_API_KEY", clienttest.Key)
	os.Setenv("CROWDVM_CONFIG_DIR", t.TempDir())
	os.Setenv("CROWDVM_NO_BROWSER", "1")
	port := os.Getenv("R8_PORT")
	os.Exit(Main([]string{"forward", id, "--remote", port, "--local", port}))
}

// `nohup crowdvm ...` starts the process with SIGHUP ignored so that it
// survives the closing of the terminal. Main registers SIGHUP with
// signal.NotifyContext; Go then installs a handler even for a signal that was
// inherited as ignored ("If the program was started with SIGHUP or SIGINT
// ignored, and Notify is called for either signal, a signal handler will be
// installed for that signal and it will no longer be ignored"). So under nohup
// (or `trap ” HUP`, `setsid`-less background jobs of a shell with huponexit
// off) a SIGHUP now cancels the CLI's context and stops whatever it ran:
// `nohup crowdvm exec ID -- long-job &` dies with the terminal, the opposite
// of what nohup is for. Main must leave SIGHUP alone when it is ignored.
func TestBughuntR8NohupIsIgnored(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cmd := exec.Command("/bin/sh", "-c", `trap '' HUP; exec "$0" -test.run '^TestBughuntR8SighupHelperProc$' -test.v`, os.Args[0])
	cmd.Env = append(os.Environ(), "R8_SIGHUP_HELPER=1", "R8_PORT="+strconv.Itoa(port))
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "forwarding") {
				ready <- true
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("helper did not start forwarding")
	}
	_ = cmd.Process.Signal(syscall.SIGHUP)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		t.Fatalf("process started with SIGHUP ignored (nohup) was stopped by SIGHUP (%v)", err)
	case <-time.After(2 * time.Second):
	}
}
