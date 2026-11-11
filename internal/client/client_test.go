package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

func testClient(t *testing.T) (*Client, *clienttest.Fake, *[]time.Duration) {
	f := clienttest.New(t)
	c := New(f.URL+"/api/v1/", clienttest.Key)
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return c, f, &waits
}

func TestNormalizeBase(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:8420":            "http://h:8420",
		"http://h:8420/api/v1/":    "http://h:8420",
		"h:8420":                   "http://h:8420",
		"https://x.example/":       "https://x.example",
		"https://x.example/api/v1": "https://x.example",
	} {
		if got := NormalizeBase(in); got != want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorEnvelope(t *testing.T) {
	c, f, _ := testClient(t)
	_, err := c.GetSandbox(context.Background(), "bx_nope")
	e, ok := AsError(err)
	if !ok || e.Status != 404 || e.Code != "sandbox_not_found" || e.RequestID != "req_test" || e.Retryable() {
		t.Fatalf("got %#v", err)
	}
	if !IsCode(err, "x", "sandbox_not_found") || !IsStatus(err, 404) {
		t.Fatal("IsCode/IsStatus")
	}
	if n := f.Count("GET /api/v1/sandboxes/bx_nope"); n != 1 {
		t.Fatalf("404 must not be retried, got %d requests", n)
	}
	bad := New(f.URL, "cvm_wrong")
	if _, err := bad.Me(context.Background()); !IsCode(err, "unauthorized") {
		t.Fatalf("want unauthorized, got %v", err)
	}
}

func TestRetryHonorsRetryAfterAndKeepsIdempotencyKey(t *testing.T) {
	c, f, waits := testClient(t)
	var n atomic.Int32
	var keys []string
	f.On("POST /api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if n.Add(1) <= 2 {
			clienttest.ErrorDetails(w, 503, "out_of_capacity", "full", map[string]any{"retryAfterSeconds": 7})
			return
		}
		w.WriteHeader(202)
		clienttest.OK(w, "sandbox.created", map[string]any{"status": "provisioning", "sandbox": map[string]any{"id": "bx_x", "state": "provisioning"}})
	})
	res, err := c.CreateSandbox(context.Background(), &CreateSandboxRequest{Type: "small", TTLSeconds: NoAutoStop()}, "")
	if err != nil || res.Sandbox.ID != "bx_x" {
		t.Fatalf("create: %v %+v", err, res)
	}
	if len(keys) != 3 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("idempotency keys: %q", keys)
	}
	if len(*waits) != 2 || (*waits)[0] != 7*time.Second {
		t.Fatalf("waits: %v", *waits)
	}
	body := f.Last("POST /api/v1/sandboxes").Body
	if !bytes.Contains(body, []byte(`"ttlSeconds":null`)) || !bytes.Contains(body, []byte(`"type":"small"`)) {
		t.Fatalf("body: %s", body)
	}
}

func TestRetryGivesUp(t *testing.T) {
	c, f, waits := testClient(t)
	c.MaxRetries = 2
	f.On("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		clienttest.Error(w, 429, "rate_limited", "slow down")
	})
	_, err := c.Me(context.Background())
	if e, ok := AsError(err); !ok || e.Code != "rate_limited" || !e.Retryable() || e.RetryAfter != 2*time.Second {
		t.Fatalf("got %v", err)
	}
	if f.Count("GET /api/v1/me") != 3 || len(*waits) != 2 {
		t.Fatalf("requests %d waits %v", f.Count("GET /api/v1/me"), *waits)
	}
}

// failFast's 503 no_ready_machine is final: no retry (Boat answers in ~1.5 s).
func TestNoReadyMachineIsNotRetried(t *testing.T) {
	c, f, waits := testClient(t)
	f.On("POST /api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 503, "no_ready_machine", "no machine is ready")
	})
	_, err := c.CreateSandbox(context.Background(), &CreateSandboxRequest{FailFast: true}, "")
	if e, ok := AsError(err); !ok || e.Code != "no_ready_machine" || f.Count("POST /api/v1/sandboxes") != 1 || len(*waits) != 0 {
		t.Fatalf("got %v, %d requests, waits %v", err, f.Count("POST /api/v1/sandboxes"), *waits)
	}
}

func TestNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway from proxy", 502)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.MaxRetries = 0
	_, err := c.Me(context.Background())
	if e, ok := AsError(err); !ok || e.Status != 502 || e.Code != "http_502" || !strings.Contains(e.Message, "bad gateway") {
		t.Fatalf("got %#v", err)
	}
}

func TestOrgHeaderAndQuery(t *testing.T) {
	c, f, _ := testClient(t)
	c.Org = "acme"
	if _, err := c.ListSandboxes(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	r := f.Last("GET /api/v1/sandboxes")
	if r.Header.Get("X-CrowdVM-Org") != "acme" || !strings.Contains(r.Query, "org=acme") {
		t.Fatalf("header %q query %q", r.Header.Get("X-CrowdVM-Org"), r.Query)
	}
}

func TestExecModes(t *testing.T) {
	c, f, _ := testClient(t)
	id := f.AddSandbox("ready")
	ctx := context.Background()
	res, err := c.Exec(ctx, id, CommandRequest{Command: "exit 3"})
	if err != nil || ExitCode(res.ExitCode) != 3 || res.Stderr != "bye\n" {
		t.Fatalf("exec: %v %+v", err, res)
	}
	st, err := c.ExecStream(ctx, id, CommandRequest{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var out string
	for {
		fr, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, fr.Type)
		if fr.Type == "stdout" {
			out += fr.Data
		}
		if fr.Type == "exit" && ExitCode(fr.ExitCode) != 0 {
			t.Fatalf("exit %v", fr.ExitCode)
		}
	}
	st.Close()
	if strings.Join(types, ",") != "started,stdout,exit" || out != "hi\n" {
		t.Fatalf("frames %v out %q", types, out)
	}
	var body map[string]any
	_ = json.Unmarshal(f.Last("POST /api/v1/sandboxes/"+id+"/commands").Body, &body)
	if body["stream"] != true {
		t.Fatalf("stream flag not sent: %v", body)
	}
	d, err := c.ExecDetached(ctx, id, CommandRequest{Command: "sleep 100"})
	if err != nil || d.ProcessID != 4242 {
		t.Fatalf("detached: %v %+v", err, d)
	}
	a, err := c.AttachCommand(ctx, id, 4242, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fr, _ := a.Recv()
	if fr.Type != "attached" {
		t.Fatalf("attach first frame %+v", fr)
	}
	a.Close()
}

func TestTunnelEcho(t *testing.T) {
	c, f, _ := testClient(t)
	id := f.AddSandbox("ready")
	conn, err := c.Tunnel(context.Background(), id, 22)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("SSH-2.0-test\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 14)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "SSH-2.0-test\r\n" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	r := f.Last("GET /api/v1/sandboxes/" + id + "/tunnel")
	if r == nil || r.Query != "port=22" || r.Header.Get("Authorization") != "Bearer "+clienttest.Key {
		t.Fatalf("tunnel request %+v", r)
	}
	if _, err := c.Tunnel(context.Background(), "bx_missing", 22); !IsCode(err, "sandbox_not_found") {
		t.Fatalf("missing sandbox: %v", err)
	}
}

func TestUploadDownload(t *testing.T) {
	c, f, _ := testClient(t)
	id := f.AddSandbox("ready")
	ctx := context.Background()
	if err := c.Upload(ctx, id, "/home/user/a.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Files[id+":/home/user/a.txt"]); got != "hello" {
		t.Fatalf("uploaded %q", got)
	}
	if r := f.Last("PUT /upload/"); r.Header.Get("Authorization") != "" {
		t.Fatal("signed upload must not carry the API key")
	}
	rc, err := c.Download(ctx, id, "/home/user/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("downloaded %q", b)
	}
	if err := c.WriteFile(ctx, id, "/home/user/b.bin", []byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ReadFile(ctx, id, "/home/user/b.bin"); err != nil || !bytes.Equal(b, []byte{0, 1, 2}) {
		t.Fatalf("read back %v %v", b, err)
	}
}

func TestUploadFallsBackWithoutSignedURLs(t *testing.T) {
	c, f, _ := testClient(t)
	id := f.AddSandbox("ready")
	f.On("POST /api/v1/sandboxes/{id}/files/upload-url", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 404, "not_found", "no signed urls here")
	})
	if err := c.Upload(context.Background(), id, "/tmp/x", strings.NewReader("via json")); err != nil {
		t.Fatal(err)
	}
	if string(f.Files[id+":/tmp/x"]) != "via json" {
		t.Fatal("fallback write missing")
	}
}

func TestDeviceLogin(t *testing.T) {
	c, f, waits := testClient(t)
	c.APIKey = ""
	f.DevicePolls = 2
	d, err := c.StartDeviceLogin(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode != "ABCD-EFGH" || d.VerificationURLComplete != f.URL+"/device?code=ABCD-EFGH" || d.ExpiresAt == "" {
		t.Fatalf("start %+v", d)
	}
	res, err := c.WaitDeviceLogin(context.Background(), d)
	if err != nil || res.APIKey != clienttest.Key || res.User.Login != "alice" {
		t.Fatalf("wait: %v %+v", err, res)
	}
	if len(*waits) != 2 || (*waits)[0] != time.Second {
		t.Fatalf("poll waits %v", *waits)
	}
	if _, err := c.Login(context.Background(), "alice", "wrong"); !IsCode(err, "invalid_credentials") {
		t.Fatalf("bad password: %v", err)
	}
	r, err := c.Login(context.Background(), "alice", "hunter2")
	if err != nil || r.APIKey != clienttest.Key {
		t.Fatalf("login %v %+v", err, r)
	}
}

func TestDevicePollDenied(t *testing.T) {
	c, f, _ := testClient(t)
	f.On("POST /auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "device.poll", map[string]any{"status": "denied"})
	})
	_, err := c.WaitDeviceLogin(context.Background(), &DeviceStart{DeviceCode: "x"})
	if !IsCode(err, CodeAccessDenied) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteSendsConfirmHeader(t *testing.T) {
	c, f, _ := testClient(t)
	id := f.AddSandbox("ready")
	op, err := c.DeleteSandbox(context.Background(), id)
	if err != nil || op.TargetID != id {
		t.Fatalf("%v %+v", err, op)
	}
	if h := f.Last("DELETE").Header; h.Get("X-Ascii-Confirm-Delete") != id {
		t.Fatal("missing boat-compatible confirm header")
	}
}

func TestTTLJSON(t *testing.T) {
	b, _ := json.Marshal(ResumeRequest{TTLSeconds: TTLSeconds(600)})
	if string(b) != `{"ttlSeconds":600}` {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(ResumeRequest{})
	if string(b) != `{}` {
		t.Fatal(string(b))
	}
}
