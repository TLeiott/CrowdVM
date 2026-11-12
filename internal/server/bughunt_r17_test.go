package server_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// A personal sandbox of an organization member runs on the organization's AI
// credential profile through the AI proxy (the member's environment names the
// profile; aiSandboxProfile lets it while the member may use it). Removing the
// member from the organization (handleRemoveMember) refuses only while their
// *org* sandboxes run, and revokes nothing else: the placeholder token of the
// member's personal sandbox keeps the org's profile and so the org's real API
// key, for as long as that sandbox runs. The next start would fall back to the
// member's own profile (aiSandboxProfile checks orgCanUse), so the running
// token is the only thing left over.
func TestBughuntR17RemovedMemberKeepsOrgAIProxyAccess(t *testing.T) {
	up := newAIUpstream(t)
	t.Setenv("CROWDVM_AI_UPSTREAM_OPENAI", up.URL)
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	bob, bobKey := env.NewUser("r17bob", false)

	r := env.Do(admin, "POST", "/api/v1/orgs", map[string]any{"name": "R17Org"})
	if r.Status != 201 {
		t.Fatalf("org: %s", r)
	}
	invite := env.Do(admin, "POST", "/api/v1/orgs/r17org/invites", map[string]any{"email": "r17bob@example.test"})
	if r := env.Do(bobKey, "POST", "/api/v1/org-invites/accept", map[string]any{"code": invite.Str("code")}); r.Status != 200 {
		t.Fatalf("accept: %s", r)
	}
	r = env.Do(admin, "POST", "/api/v1/credential-profiles", map[string]any{"name": "orgprof", "org": "r17org"})
	if r.Status != 201 {
		t.Fatalf("org profile: %s", r)
	}
	prof := r.Str("profile.id")
	const realKey = "sk-openai-ORG-REAL-KEY"
	if r := env.Do(admin, "PUT", "/api/v1/credential-profiles/"+prof+"/keys/openai-api-key", map[string]any{"value": realKey}); r.Status != 200 {
		t.Fatalf("org key: %s", r)
	}
	if r := env.Do(admin, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true, "org": "r17org"}); r.Status != 200 {
		t.Fatalf("org proxy: %s", r)
	}
	if r := env.Do(bobKey, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": prof}); r.Status != 200 {
		t.Fatalf("bob's environment cannot name the org profile: %s", r)
	}
	id := createSandbox(t, env, bobKey, nil)
	env.WaitState(bobKey, id, "ready")
	gc := guestSpec(t, node, id)
	if gc.AIProxy == nil {
		t.Fatalf("setup: the personal sandbox of an org member got no AI proxy token: %+v", gc.Env)
	}
	tok := gc.AIProxy.Token
	call := func() int {
		resp := proxyReq(t, env, "POST", "/ai-proxy/openai/v1/chat/completions", `{"model":"gpt-6.1-sol"}`, "Authorization", "Bearer "+tok)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call(); code != 200 || up.last().Header.Get("Authorization") != "Bearer "+realKey {
		t.Fatalf("setup: the member's proxy call answered %d (%v)", code, up.last().Header)
	}
	if r := env.Do(admin, "DELETE", "/api/v1/orgs/r17org/members/"+bob.ID, nil); r.Status != 200 {
		t.Fatalf("remove member: %s", r)
	}
	up.mu.Lock()
	before := len(up.seen)
	up.mu.Unlock()
	code := call()
	up.mu.Lock()
	reached := len(up.seen) > before
	up.mu.Unlock()
	if code == http.StatusOK || reached {
		t.Fatalf("a removed member's sandbox still reaches the organization's OpenAI key through the AI proxy (answer %d, upstream reached: %v)", code, reached)
	}
}

// The coordinator keeps a copy of the guest's agent events and serves it when
// the sandbox does not run (feat_history.go agentEvents). It orders them by
// (ts, event_id). Event ids are random and a fast prompt (a harness that is not
// installed, a refused credential) emits "running" and "failed" in the same
// millisecond, so the stored order of equal timestamps is a coin flip. The
// overlay takes the *last* prompt.status in that order: when "running" sorts
// last, a finished prompt is shown as "interrupted: the sandbox stopped while
// this prompt was running" and its error is lost.
func TestBughuntR17StoredPromptStatusOrderOfSameMillisecond(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	pt := time.Now().UnixMilli()
	node.SendGuestEvent(id, "prompt.event", map[string]any{"id": "prompt_r17", "type": "prompt", "timestamp": pt, "taskId": "prompt_r17", "conversationId": "conv_r17",
		"data": map[string]any{"prompt": "fix it", "status": "queued", "provider": "claude"}})
	// Emitted in this order, in one millisecond; the random ids sort the other way round.
	node.SendGuestEvent(id, "prompt.event", map[string]any{"id": "pst_zzzzzzzz", "type": "prompt.status", "timestamp": pt + 1, "taskId": "prompt_r17", "conversationId": "conv_r17",
		"data": map[string]any{"status": "running"}})
	node.SendGuestEvent(id, "prompt.event", map[string]any{"id": "pst_aaaaaaaa", "type": "prompt.status", "timestamp": pt + 1, "taskId": "prompt_r17", "conversationId": "conv_r17",
		"data": map[string]any{"status": "failed", "error": "provider_not_configured"}})
	env.Eventually("events stored", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM agent_events WHERE sandbox_id=?`, id).Scan(&n)
		return n == 3
	})
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/prompts/prompt_r17", nil)
	if r.Str("promptRun.status") != "failed" {
		t.Fatalf("a prompt that failed is served as %q (%s) after the stop", r.Str("promptRun.status"), r.Str("promptRun.error"))
	}
	ev := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/events?sort=asc", nil)
	if ev.Str("events.1.id") != "pst_zzzzzzzz" || ev.Str("events.2.id") != "pst_aaaaaaaa" {
		t.Fatalf("the stored events of one millisecond are not in emission order: %s", ev)
	}
}

// handlePrompt fills a missing provider from the account's default
// (applyPromptDefaults) before the guest sees the request, even when the
// prompt continues an existing conversation. The guest's own rule for an
// omitted provider is "the conversation's last harness" (agents/http.go:
// c.LastHarness), so with an account default of codex, a follow-up to a
// Claude Code conversation (no provider given) silently moves the
// conversation to Codex: the Claude session is dropped and the history is
// replayed as text into the other agent.
func TestBughuntR17AccountDefaultProviderDoesNotHijackAConversation(t *testing.T) {
	env := servertest.New(t)
	g := &promptGuest{FakeGuest: servertest.NewFakeGuest()}
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": "sk-openai"}); r.Status != 200 {
		t.Fatalf("key: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "sk-ant"}); r.Status != 200 {
		t.Fatalf("key: %s", r)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"agentDefaults": map[string]any{"provider": "codex"}}); r.Status != 200 {
		t.Fatalf("settings: %s", r)
	}
	// A follow-up to an existing conversation, no provider given.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "go on", "conversationId": "conv-started-with-claude"}); r.Status != 202 {
		t.Fatalf("prompt: %s", r)
	}
	if p, ok := g.last()["provider"]; ok {
		t.Fatalf("the server named provider %q for a continued conversation; the guest would have kept the conversation's harness", p)
	}
}
