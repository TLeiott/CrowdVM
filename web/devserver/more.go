package main

// Mocks for the endpoints the dashboard gained after the first devserver:
// notices, account settings, metrics, OpenAPI, CI, data retention, admin.
// Shapes copied from a real crowdvm-server.

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
)

func moreRoutes(mux *http.ServeMux) {
	notices := []M{{"id": "ntc_demo1", "kind": "credits_low", "level": "warning", "title": "Credits are low",
		"message": "Your wallet covers about 3 more hours of a default sandbox.", "createdAt": ts(ago(2 * time.Hour)), "dismissedAt": nil}}
	mux.HandleFunc("GET /api/v1/notices", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "notice.list", M{"notices": notices})
	}))
	mux.HandleFunc("POST /api/v1/notices/{nid}/dismiss", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		notices = without(notices, "id", r.PathValue("nid"))
		ok(w, 200, "notice.dismissed", nil)
	}))
	mux.HandleFunc("POST /api/v1/notices/dismiss-all", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		notices = nil
		ok(w, 200, "notice.dismissed", nil)
	}))

	settings := M{"agentDefaults": M{"provider": "claude", "model": nil, "fast": false}, "useCase": nil, "notifyEmail": nil, "onboarded": true, "safeForThirdPartiesDefault": true}
	mux.HandleFunc("GET /api/v1/account-settings", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "account.settings", settings)
	}))
	mux.HandleFunc("PATCH /api/v1/account-settings", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		for k, v := range body(r) {
			settings[k] = v
		}
		ok(w, 200, "account.settings", settings)
	}))
	mux.HandleFunc("GET /api/v1/provider-models", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		model := func(id, name string) M {
			return M{"id": id, "name": name, "fastMode": false, "reasoningEfforts": []string{"low", "medium", "high"}}
		}
		ok(w, 200, "provider_models", M{"providers": []M{
			{"provider": "claude", "name": "Claude Code", "binary": "claude", "configured": true, "defaultModel": "claude-opus-5-5", "steer": "native",
				"models": []M{model("claude-opus-5-5", "Claude Opus 5.5"), model("claude-sonnet-5-5", "Claude Sonnet 5.5")}},
			{"provider": "codex", "name": "Codex", "binary": "codex", "configured": false, "defaultModel": "gpt-6.1-sol", "steer": "native",
				"models": []M{model("gpt-6.1-sol", "GPT-6.1 Sol")}},
		}})
	}))
	mux.HandleFunc("GET /api/v1/repos/sources", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "git_source.list", M{"sources": []M{}})
	}))
	mux.HandleFunc("GET /api/v1/telemetry", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "telemetry.info", M{"telemetry": nil})
	}))
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/snapshots/latest", authed(withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		if s.Snap.IsZero() {
			fail(w, 404, "snapshot_not_found", "This sandbox has no snapshot yet.")
			return
		}
		ok(w, 200, "snapshot.latest", M{"snapshot": M{"id": "snap_" + s.ID[3:], "sandboxId": s.ID, "status": "completed", "kind": "incremental",
			"trigger": "periodic", "createdAt": ts(s.Snap), "completedAt": ts(s.Snap), "sizeBytes": 734003200, "uploaded": true}})
	})))

	// Metrics: a smooth fake series every 30 s while the sandbox "ran"
	// (the last 3 days, minus an 8-hour stop the night before).
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/metrics", authed(withSB(func(w http.ResponseWriter, r *http.Request, s *sandbox) {
		since := time.Now().Add(-time.Hour)
		if t, err := time.Parse(time.RFC3339, r.URL.Query().Get("from")); err == nil {
			since = t
		}
		if lim := time.Now().Add(-7 * 24 * time.Hour); since.Before(lim) {
			since = lim
		}
		end := time.Now()
		if s.State == "archived" || s.State == "stopped" {
			end = s.Stopped
		}
		start := s.Created
		if t := time.Now().Add(-72 * time.Hour); start.Before(t) {
			start = t
		}
		gapFrom, gapTo := time.Now().Add(-20*time.Hour), time.Now().Add(-12*time.Hour)
		step := max(30*time.Second, (time.Since(since) / 360).Round(time.Second)) // at most 360 points, like the server
		const memTotal, diskTotal = 8 << 30, 40 << 30
		var out []M
		seed := float64(len(s.ID))
		for t := since.Truncate(step); t.Before(end); t = t.Add(step) {
			if t.Before(start) || (t.After(gapFrom) && t.Before(gapTo)) {
				continue
			}
			x := float64(t.Unix()) / 600
			cpu := 0.18 + 0.14*math.Sin(x+seed) + 0.08*math.Sin(3.7*x) + 0.25*math.Max(0, math.Sin(x/5))
			out = append(out, M{"at": ts(t), "samples": int(step / (30 * time.Second)),
				"cpuUtilization":   math.Max(0.01, math.Min(0.99, cpu)),
				"memoryUsedBytes":  int64(float64(memTotal) * (0.32 + 0.1*math.Sin(x/3+seed))),
				"memoryTotalBytes": memTotal,
				"diskUsedBytes":    int64(float64(diskTotal) * (0.21 + 0.03*float64(t.Sub(start))/float64(72*time.Hour))),
				"diskTotalBytes":   diskTotal,
				"ioPressure":       math.Max(0, 0.03+0.04*math.Sin(2.3*x+seed)),
			})
		}
		ok(w, 200, "sandbox.metrics", M{"id": s.ID, "from": ts(since), "to": ts(time.Now()), "stepSeconds": int(step.Seconds()),
			"intervalSeconds": 30, "live": s.State != "archived" && s.State != "stopped", "points": out})
	})))

	mux.HandleFunc("GET /api/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, openapiMock()) })
	mux.HandleFunc("GET /openapi/crowdvm-v1.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte("# The devserver serves JSON only: /api/v1/openapi.json\n"))
	})

	mux.HandleFunc("GET /api/v1/ci", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		var perDay []M
		for i := 6; i >= 0; i-- {
			perDay = append(perDay, M{"day": time.Now().AddDate(0, 0, -i).Format("2006-01-02"), "success": 3 + i%3, "failure": i % 2, "other": 0})
		}
		ok(w, 200, "ci.overview", M{"counts": M{"active": 0, "all": 0, "failed": 0}, "filter": M{"account": nil, "repo": nil, "state": "all"},
			"installations": []M{}, "jobs": []M{},
			"setup": M{"complete": false, "next": "install", "problems": []M{}, "steps": []M{
				{"id": "install", "title": "Connect GitHub with a token", "detail": "Run `crowdvm ci connect --token <GitHub token>`.", "done": false},
				{"id": "repositories", "title": "Repositories visible to the token", "detail": "0 repositories.", "done": false},
				{"id": "workflow", "title": "Use runs-on: crowdvm in a workflow", "detail": "runs-on: crowdvm (or crowdvm-small, -large, -xlarge).", "done": false},
				{"id": "first-pass", "title": "A job passed", "detail": "The first green job on CrowdVM.", "done": false}}},
			"stats": M{"costDollars": 0, "jobs": 0, "medianRunSeconds": nil, "medianWaitSeconds": nil, "perDay": perDay}})
	}))

	retention := M{"enabled": false, "enabledAt": nil, "monitoringOff": false, "monitoringOffAt": nil, "snapshotsOff": false, "snapshotsOffAt": nil}
	mux.HandleFunc("GET /api/v1/account/data-retention", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "data_retention.info", retention)
	}))
	mux.HandleFunc("PATCH /api/v1/account/data-retention", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		for k, v := range body(r) {
			retention[k] = v
		}
		ok(w, 200, "data_retention.info", retention)
	}))
	mux.HandleFunc("GET /api/v1/account/connections", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "account.connections", M{"connections": []M{{"login": u.Login, "method": "password", "primary": true}}, "primary": "password", "notificationEmail": nil})
	}))

	mux.HandleFunc("GET /api/v1/admin/users", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		var list []M
		for _, x := range users {
			list = append(list, M{"id": x.ID, "login": x.Login, "username": x.Login, "displayName": x.Display, "isAdmin": x.Login == "demo",
				"kind": "user", "accountStatus": "active", "credits": 42.5, "activeSandboxes": len(sandboxes), "createdAt": ts(x.Created),
				"closed": false, "suspended": false, "connectionMethods": []string{"password"}})
		}
		ok(w, 200, "admin.users", M{"users": list})
	}))
	mux.HandleFunc("GET /api/v1/admin/invite-codes", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "admin.invite_codes", M{"inviteCodes": []M{}})
	}))
	mux.HandleFunc("GET /api/v1/admin/retention", authed(func(w http.ResponseWriter, r *http.Request, u *user) {
		ok(w, 200, "retention.config", M{"retention": M{"mode": "enforce", "olderHours": 24, "stoppedDays": 14, "lastRunAt": nil, "lastReport": nil}})
	}))
}

// A short OpenAPI document in the server's format (the real one is generated
// from its route table and lists every route).
func openapiMock() M {
	env := M{"2XX": M{"description": "Success envelope ({ok: true, type, ...fields}).", "content": M{"application/json": M{"schema": M{"$ref": "#/components/schemas/Envelope"}}}}}
	pathP := func(n string) M { return M{"in": "path", "name": n, "required": true, "schema": M{"type": "string"}} }
	queryP := func(n string) M { return M{"in": "query", "name": n, "required": false, "schema": M{"type": "string"}} }
	ops := []struct {
		method, path, summary, action string
		params                        []M
		body, ext                     bool
	}{
		{"get", "/me", "Get the current account", "account.read", nil, false, false},
		{"get", "/sandboxes", "List sandboxes", "sandbox.read", []M{queryP("limit"), queryP("state"), queryP("label")}, false, false},
		{"post", "/sandboxes", "Create a sandbox", "sandbox.create", nil, true, false},
		{"get", "/sandboxes/{sandboxId}", "Get sandbox", "sandbox.read", []M{pathP("sandboxId")}, false, false},
		{"delete", "/sandboxes/{sandboxId}", "Permanently delete sandbox data", "sandbox.delete", []M{pathP("sandboxId"), {"in": "header", "name": "X-CrowdVM-Confirm-Delete", "required": false, "schema": M{"type": "string"}}}, false, false},
		{"post", "/sandboxes/{sandboxId}/stop", "Stop a sandbox", "sandbox.stop", []M{pathP("sandboxId")}, true, false},
		{"post", "/sandboxes/{sandboxId}/commands", "Execute a command in a sandbox", "exec", []M{pathP("sandboxId")}, true, false},
		{"get", "/sandboxes/{sandboxId}/metrics", "Sandbox CPU, memory and disk samples", "sandbox.read", []M{pathP("sandboxId"), queryP("from"), queryP("to"), queryP("step")}, false, true},
		{"get", "/nodes", "List nodes", "nodes.read", nil, false, true},
		{"get", "/credits", "Credit balance and ledger", "credits.read", []M{queryP("days")}, false, true},
	}
	paths := M{}
	for _, o := range ops {
		item, _ := paths[o.path].(M)
		if item == nil {
			item = M{}
			paths[o.path] = item
		}
		op := M{"summary": o.summary, "operationId": strings.ReplaceAll(o.method+o.path, "/", "_"), "x-crowdvm-action": o.action, "responses": env, "tags": []string{"Account"}}
		if o.params != nil {
			op["parameters"] = o.params
		}
		if o.body {
			op["requestBody"] = M{"required": false, "content": M{"application/json": M{"schema": M{"type": "object", "additionalProperties": true}}}}
		}
		if o.ext {
			op["tags"] = []string{"CrowdVM"}
			op["x-crowdvm-extension"] = true
		}
		item[o.method] = op
	}
	return M{"openapi": "3.1.0", "info": M{"title": "CrowdVM API v1 (devserver mock)", "version": "dev"},
		"servers": []M{{"url": "/api/v1"}}, "paths": paths,
		"components": M{"schemas": M{"Envelope": M{"type": "object", "additionalProperties": true}}}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
