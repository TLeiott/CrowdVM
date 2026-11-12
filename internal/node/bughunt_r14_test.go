package node

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bugs-r14-sonnet 3: the page signs "<method> <path+query> <ms>" and nothing
// else (bugs-r13-opus 5), so the body of a signed request is free. A
// squatter on the UI port while the daemon is down (the threat the fix
// answers) sees the page's signed POSTs and can send the captured header
// within the 60 s window with a body of its own: the Pause click
// ({"paused":true}) becomes a resume, a config change becomes another value
// of any key the page may set. The r13 comment says the squatter "sees only
// requests it cannot turn into other actions"; with the body outside the
// signature it can.
func TestBughuntR14SignedRequestBodyIsNotCovered(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	sess := loginSession(t, newLogin(t, srv.URL, "tok"))
	nonce, key, _ := strings.Cut(sess, ".")
	ms := strconv.FormatInt(time.Now().UnixMilli(), 10)
	header := nonce + "." + ms + "." + uiReqSig(key, "POST", "/api/pause", ms, []byte(`{"paused":true}`)) // what the page sent for "pause"

	post := func(body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/pause", strings.NewReader(body))
		req.Header.Set(UIHeader, "1")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(UISessionHeader, header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// The squatter sends the captured header first, with its own body.
	if c := post(`{"paused":false}`); c != 401 {
		t.Fatalf("the captured signature was accepted for a body the page never sent: %d (want 401)", c)
	}
	if c := post(`{"paused":true}`); c != 200 {
		t.Fatalf("the page's own request: %d", c)
	}
	if c := post(`{"paused":true}`); c != 401 {
		t.Fatalf("the same signed request was accepted twice: %d (want 401)", c)
	}
}
