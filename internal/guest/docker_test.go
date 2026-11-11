package guest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// fakeDocker installs a docker CLI stand-in. State: one "<id> <running> <name>"
// line per container, newest last; every call is appended to calls.log.
func fakeDocker(t *testing.T, containers string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "state"), []byte(containers), 0o644)
	os.WriteFile(filepath.Join(dir, "sock"), nil, 0o644)
	script := `#!/bin/bash
D=` + dir + `
echo "$*" >> $D/calls.log
case "$1" in
info) exit 0 ;;
ps)   tac $D/state | awk '$2=="true"{print $1}' ;;
stop) shift 3; for id in "$@"; do sed -i "s/^$id true/$id false/" $D/state; done ;;
start) sed -i "s/^$2 false/$2 true/" $D/state ;;
container) shift 4; for id in "$@"; do awk -v id="$id" '$1==id{print $1, $2, "/" $3}' $D/state; done ;;
builder) exit 0 ;;
*) exit 1 ;;
esac
`
	bin := filepath.Join(dir, "docker")
	os.WriteFile(bin, []byte(script), 0o755)
	oldBin, oldSock, oldWait := dockerBin, dockerSock, dockerWait
	dockerBin, dockerSock, dockerWait = bin, filepath.Join(dir, "sock"), 5*time.Second
	t.Cleanup(func() { dockerBin, dockerSock, dockerWait = oldBin, oldSock, oldWait })
	return dir
}

func TestDockerRunningContainersComeBack(t *testing.T) {
	root := t.TempDir()
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	e := newEnvAt(t, proto.GuestConfig{}, home, data)
	d := fakeDocker(t, "c1 true db\nc2 false old\nc3 true web\n")

	m := e.mustOK("POST", "/v1/internal/prepare-stop", nil)
	if num(m["dockerStopped"]) != 2 {
		t.Fatalf("dockerStopped %v", m)
	}
	var st dockerState
	b, _ := os.ReadFile(filepath.Join(data, dockerStateFile))
	if json.Unmarshal(b, &st) != nil || !slices.Equal(st.Containers, []string{"c1", "c3"}) {
		t.Fatalf("recorded %s", b)
	}
	if s, _ := os.ReadFile(filepath.Join(d, "state")); strings.Contains(string(s), "true") {
		t.Fatalf("not stopped: %s", s)
	}

	// Next boot from this disk (resume): the recorded ones start again,
	// oldest first; c2 never ran and stays stopped.
	started, failed := e.a.restartDocker(context.Background())
	if !slices.Equal(started, []string{"db", "web"}) || len(failed) != 0 {
		t.Fatalf("started %v failed %v", started, failed)
	}
	if s, _ := os.ReadFile(filepath.Join(d, "state")); string(s) != "c1 true db\nc2 false old\nc3 true web\n" {
		t.Fatalf("state %s", s)
	}
	// A container Docker already started (restart policy) is left alone.
	calls, _ := os.ReadFile(filepath.Join(d, "calls.log"))
	before := strings.Count(string(calls), "start ")
	e.a.restartDocker(context.Background())
	calls, _ = os.ReadFile(filepath.Join(d, "calls.log"))
	if strings.Count(string(calls), "start ") != before {
		t.Fatalf("started running containers again:\n%s", calls)
	}
}

func TestDockerRecordedBeforeLiveSnapshot(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	d := fakeDocker(t, "c1 true api\n")
	e.mustOK("POST", "/v1/internal/freeze", nil)
	e.mustOK("POST", "/v1/internal/thaw", nil)
	b, _ := os.ReadFile(filepath.Join(e.data, dockerStateFile))
	if !strings.Contains(string(b), `"c1"`) {
		t.Fatalf("freeze did not record: %s", b)
	}
	if s, _ := os.ReadFile(filepath.Join(d, "state")); !strings.Contains(string(s), "c1 true") {
		t.Fatal("a live snapshot stopped a container")
	}
	// unchanged list: no rewrite
	fi1, _ := os.Stat(filepath.Join(e.data, dockerStateFile))
	time.Sleep(20 * time.Millisecond)
	e.mustOK("POST", "/v1/internal/freeze", nil)
	e.mustOK("POST", "/v1/internal/thaw", nil)
	fi2, _ := os.Stat(filepath.Join(e.data, dockerStateFile))
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("unchanged container list rewritten")
	}
}

func TestUnusedImages(t *testing.T) {
	ls := "sha256:a nginx:alpine\nsha256:b node:22\nsha256:c <none>:<none>\nsha256:d myproj-web:latest\n" +
		"sha256:e redis:<none>\nsha256:f postgres:16\nsha256:f pg:mine\n"
	keep := map[string]bool{"sha256:a": true, normImage("docker.io/library/myproj-web"): true, "sha256:f": true}
	got := unusedImages(ls, keep)
	if !slices.Equal(got, []string{"node:22", "sha256:c", "sha256:e"}) {
		t.Fatalf("unused %v", got)
	}
	// Classic store: built images show no digest.
	ls = "sha256:a nginx:alpine sha256:111\nsha256:d myproj-web:latest <none>\nsha256:c <none>:<none> <none>\n"
	classic := func(_, d string) bool { return strings.HasPrefix(d, "sha256:") }
	if got := imageLines(ls, classic); got != "sha256:a nginx:alpine\n" {
		t.Fatalf("pulled only %q", got)
	}
	if got := imageLines(ls, nil); got != "sha256:a nginx:alpine\nsha256:d myproj-web:latest\nsha256:c <none>:<none>\n" {
		t.Fatalf("all %q", got)
	}
	// containerd store: every image has a digest; only pulls carry a source label.
	src := registrySourced("DIGEST\tSIZE\tAGE\tLABELS\n" +
		"sha256:f59a\t9.2kB\t4 seconds\tcontainerd.io/distribution.source.docker.io=library/alpine,containerd.io/gc.ref.content.m.0=sha256:0f49\n" +
		"sha256:4bd9\t855B\tLess than a second\tcontainerd.io/gc.ref.content.0=sha256:5c61\n")
	ls = "sha256:4bd9 mybuilt:1 sha256:4bd9\nsha256:f59a alpine:3 sha256:f59a\n"
	if got := imageLines(ls, func(id, _ string) bool { return src[id] }); got != "sha256:f59a alpine:3\n" {
		t.Fatalf("containerd pulled only %q", got)
	}
	for in, want := range map[string]string{"nginx": "nginx:latest", "docker.io/library/nginx:1": "nginx:1",
		"localhost:5000/app": "localhost:5000/app:latest", "ghcr.io/x/y:2": "ghcr.io/x/y:2", "a@sha256:00": "a@sha256:00"} {
		if got := normImage(in); got != want {
			t.Errorf("normImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBootFromLiveSnapshotCleansUnsaved(t *testing.T) {
	root := t.TempDir()
	home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
	newEnvAt(t, proto.GuestConfig{SandboxID: "bx_src"}, home, data) // the source sandbox's boot
	os.MkdirAll(filepath.Join(home, ".cache/pip"), 0o755)
	os.WriteFile(filepath.Join(home, ".cache/pip/x"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(home, "proj/dist"), 0o755)
	os.WriteFile(filepath.Join(home, "proj/.crowdvmignore"), []byte("dist/\n"), 0o644)
	os.WriteFile(filepath.Join(home, "proj/keep.txt"), []byte("k"), 0o644)

	// A fork of the running source: no prepare-stop ran on this disk.
	newEnvAt(t, proto.GuestConfig{SandboxID: "bx_fork", Boot: proto.BootFork}, home, data)
	if ents, err := os.ReadDir(filepath.Join(home, ".cache")); err != nil || len(ents) != 0 {
		t.Fatalf("~/.cache after fork boot: %v %v", ents, err)
	}
	if _, err := os.Stat(filepath.Join(home, "proj/dist")); err == nil {
		t.Fatal("ignored folder kept")
	}
	if _, err := os.Stat(filepath.Join(home, "proj/keep.txt")); err != nil {
		t.Fatal("kept file deleted")
	}

	// A resume after a clean stop: prepare-stop cleaned, the boot does not.
	e := newEnvAt(t, proto.GuestConfig{SandboxID: "bx_res"}, home, data)
	e.mustOK("POST", "/v1/internal/prepare-stop", nil)
	os.MkdirAll(filepath.Join(home, ".cache/after"), 0o755)
	newEnvAt(t, proto.GuestConfig{SandboxID: "bx_res2", Boot: proto.BootResume}, home, data)
	if _, err := os.Stat(filepath.Join(home, ".cache/after")); err != nil {
		t.Fatal("clean-stopped disk cleaned again at boot")
	}
}

// A stop that fails after prepare-stop thaws the machine: the containers
// that prepare-stop stopped run again (the sandbox stays usable).
func TestDockerBackAfterFailedStop(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	d := fakeDocker(t, "c1 true db\n")
	e.mustOK("POST", "/v1/internal/prepare-stop", nil)
	if s, _ := os.ReadFile(filepath.Join(d, "state")); string(s) != "c1 false db\n" {
		t.Fatalf("not stopped: %s", s)
	}
	e.mustOK("POST", "/v1/internal/thaw", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := os.ReadFile(filepath.Join(d, "state")); string(s) == "c1 true db\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("container not started again after the failed stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// a thaw after a live snapshot (no prepare-stop) starts nothing
	calls, _ := os.ReadFile(filepath.Join(d, "calls.log"))
	e.mustOK("POST", "/v1/internal/thaw", nil)
	time.Sleep(100 * time.Millisecond)
	if after, _ := os.ReadFile(filepath.Join(d, "calls.log")); string(after) != string(calls) {
		t.Fatalf("plain thaw touched docker:\n%s", after[len(calls):])
	}
}
