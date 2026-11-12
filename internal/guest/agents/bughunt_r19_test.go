package agents

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Round 18 clears m.events at the start of an agent with zero data
// retention, but load() fills m.prompts and m.convs from state.json AFTER that
// block and rewrites the file without the text: the in-memory records keep
// the earlier conversation previews (lastPromptPreview), and GET
// /v1/conversations serves them until the agent restarts.
func TestBughuntR19ZeroRetentionStillServesEarlierPromptsFromMemory(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "r19-earlier-secret-prompt"})
	e.waitStatus(p, "finished")
	time.Sleep(200 * time.Millisecond)
	// The next boot of the same disk has zero data retention on.
	m := New(Options{Home: t.TempDir(), Unprivileged: true, DataDir: dir, SandboxID: "bx_test",
		ZeroDataRetention: func() bool { return true }})
	mux := http.NewServeMux()
	m.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, path := range []string{"/v1/prompts/" + p, "/v1/conversations"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if strings.Contains(sb.String(), "r19-earlier-secret-prompt") {
			t.Fatalf("GET %s serves the text of a prompt kept from before zero data retention: %s", path, sb.String())
		}
	}
}
