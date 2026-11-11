package server

import (
	"net/http"
	"strconv"
)

// routes registers the core API. Paths are relative to /api/v1 unless they
// start with /auth, /node, /agent, /oauth2 or /.well-known.
func (s *Server) routes() {
	s.authRoutes()
	s.nodeRoutes()
	s.apiKeyRoutes()
	s.orgRoutes()
	s.creditRoutes()
	s.adminRoutes()
	s.passthroughRoutes()

	s.API("GET /me", "account.read", s.handleMe)
	s.API("GET /limits", "account.read", s.handleLimits)
	s.API("GET /server", ActionAny, s.handleServerInfo)
	// Unauthenticated liveness probe for load balancers and monitors.
	s.router.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := s.DB.PingContext(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false,"error":"database unavailable"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"version":` + strconv.Quote(Version) + `}` + "\n"))
	}))
	s.API("GET /deletion-operations/{operationId}", "account.read", s.handleGetDeletionOp)

	// Sandboxes.
	s.API("GET /sandboxes", "sandbox.read", s.handleListSandboxes)
	s.API("POST /sandboxes", "sandbox.create", s.handleCreateSandbox)
	s.API("GET /sandboxes/{sandboxId}", "sandbox.read", s.handleGetSandbox)
	s.API("PATCH /sandboxes/{sandboxId}", "sandbox.update", s.handleUpdateSandbox)
	s.API("DELETE /sandboxes/{sandboxId}", "sandbox.delete", s.handleDeleteSandbox)
	s.API("POST /sandboxes/{sandboxId}/stop", "sandbox.stop", s.handleStop)
	s.API("GET /sandboxes/{sandboxId}/stops/{stopId}", "sandbox.read", s.handleGetStop)
	s.API("POST /sandboxes/{sandboxId}/resume", "sandbox.resume", s.handleResume)
	s.API("POST /sandboxes/{sandboxId}/fork", "sandbox.fork", s.handleFork)
	s.API("POST /sandboxes/{sandboxId}/recover", "sandbox.update", s.handleRecover)
	s.API("POST /sandboxes/{sandboxId}/share", "sandbox.update", s.handleShare)
	s.API("POST /sandboxes/{sandboxId}/unshare", "sandbox.update", s.handleUnshare)
	s.API("GET /sandboxes/{sandboxId}/usage", "sandbox.read", s.handleSandboxUsage)

	// Snapshots.
	s.API("GET /snapshots", "snapshot.read", s.handleListSnapshots)
	s.API("GET /sandboxes/{sandboxId}/snapshots", "snapshot.read", s.handleListSandboxSnapshots)
	s.API("DELETE /sandboxes/{sandboxId}/snapshots", "snapshot.write", s.handleDeleteSandboxSnapshots)
	s.API("GET /sandboxes/{sandboxId}/snapshots/latest", "snapshot.read", s.handleLatestSnapshot)
	s.API("DELETE /snapshots/{snapshotId}", "snapshot.write", s.handleDeleteSnapshot)
	s.API("GET /saved-snapshots", "snapshot.read", s.handleListSaved)
	s.API("POST /saved-snapshots", "snapshot.write", s.handleSaveSaved)
	s.API("GET /saved-snapshots/{name}", "snapshot.read", s.handleGetSaved)
	s.API("DELETE /saved-snapshots/{name}", "snapshot.write", s.handleDeleteSaved)
	s.API("GET /saved-snapshots/{name}/shares", "snapshot.read", s.handleListShares)
	s.API("POST /saved-snapshots/{name}/shares", "snapshot.write", s.handleCreateShare)
	s.API("DELETE /saved-snapshots/{name}/shares/{shareId}", "snapshot.write", s.handleRevokeShare)
	// Deprecated Boat aliases.
	s.API("GET /named-snapshots", "snapshot.read", s.handleListSaved)
	s.API("POST /named-snapshots", "snapshot.write", s.handleSaveSaved)
	s.API("GET /named-snapshots/{name}", "snapshot.read", s.handleGetSaved)
	s.API("DELETE /named-snapshots/{name}", "snapshot.write", s.handleDeleteSaved)

	// Implemented by later features; until then they answer 501.
	notYet := func(pattern, what string) {
		s.router.Stub(pattern, 501, "not_implemented", what+" is not available on this server yet.")
	}
	notYet("GET /api/v1/snapshots/{snapshotId}/tree", "Snapshot file trees")
	notYet("GET /api/v1/snapshots/{snapshotId}/files", "Snapshot file downloads")
	notYet("GET /api/v1/snapshots/{snapshotId}/download", "Snapshot downloads")
	notYet("POST /api/v1/sandboxes/{sandboxId}/host", "Public URLs")
	notYet("POST /api/v1/sandboxes/{sandboxId}/desktop", "Desktop streaming")
	for _, p := range []string{"environments", "webhooks", "telemetry", "credential-profiles", "ai-proxy", "ai-usage", "ci", "repos", "secrets", "account/data-retention"} {
		notYet("/api/v1/"+p, p)
		notYet("/api/v1/"+p+"/{rest...}", p)
	}
	notYet("/api/v1/sandboxes/{sandboxId}/ai-usage", "AI usage")
	notYet("/api/v1/sandboxes/{sandboxId}/ai-access", "AI access")
	// Unknown API routes: JSON 404.
	s.router.Stub("/api/v1/", 404, "not_found", "Unknown API route.")
}
