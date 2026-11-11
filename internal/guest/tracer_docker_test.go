package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// CMD-021: containers with Docker's default json-file driver become
// process records.
func TestTracerDockerJSONFile(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{Boot: proto.BootCreate})
	dir := t.TempDir()
	old := filepath.Join(dir, "aaa111", "aaa111-json.log")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte(`{"log":"before start\n","stream":"stdout","time":"x"}`+"\n"), 0o644)
	dockerPoll = 20 * time.Millisecond
	stop := make(chan struct{})
	defer close(stop)
	go e.a.tracer.followDocker(stop, dir)
	time.Sleep(100 * time.Millisecond)
	// a new container, and new output of the old one
	nc := filepath.Join(dir, "bbb222")
	os.MkdirAll(nc, 0o755)
	os.WriteFile(filepath.Join(nc, "config.v2.json"), []byte(`{"Name":"/web","Config":{"Image":"nginx:1"}}`), 0o644)
	os.WriteFile(filepath.Join(nc, "bbb222-json.log"), []byte(`{"log":"hello\n","stream":"stdout"}`+"\n"+`{"log":"oops\n","stream":"stderr"}`+"\n"+`{"log":"partial`), 0o644)
	f, _ := os.OpenFile(old, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"log":"after start\n","stream":"stdout"}` + "\n")
	f.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var web, old *meta
		for _, m := range e.a.hist.all() {
			switch {
			case m.Command == "docker container web (nginx:1)":
				web = m
			case strings.HasPrefix(m.Command, "docker container aaa111"):
				old = m
			}
		}
		if web != nil && old != nil {
			out, _ := os.ReadFile(e.a.hist.path(web.CommandID, "stdout"))
			errOut, _ := os.ReadFile(e.a.hist.path(web.CommandID, "stderr"))
			oldOut, _ := os.ReadFile(e.a.hist.path(old.CommandID, "stdout"))
			if string(out) == "hello\n" && string(errOut) == "oops\n" && string(oldOut) == "after start\n" && web.Mode == "process" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("outputs %q %q %q", out, errOut, oldOut)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("no records: web=%v old=%v", web, old)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTracerQuietRunningContainer(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{})
	id := strings.Repeat("a", 64)
	dir := fakeDocker(t, id+" true web\n")
	tr := e.a.tracer
	tr.write("ctr:"+id[:12], 0, "docker container web", "stdout", "hello\n")
	tr.mu.Lock()
	tr.recs["ctr:"+id[:12]].seen = time.Now().Add(-11 * time.Minute)
	m := tr.recs["ctr:"+id[:12]].m
	tr.mu.Unlock()
	tr.reap(time.Now())
	if m.Status != "running" {
		t.Fatalf("quiet running container marked %s", m.Status)
	}
	// A daemon failure must also preserve the record.
	os.Remove(filepath.Join(dir, "docker"))
	tr.reap(time.Now())
	if m.Status != "running" {
		t.Fatalf("daemon failure marked container %s", m.Status)
	}
	// Restore the stand-in with an empty running set.
	fakeDocker(t, "")
	tr.reap(time.Now())
	if m.Status != "exited" {
		t.Fatalf("stopped container stays %s", m.Status)
	}
}
