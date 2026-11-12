package guest

import (
	"os"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bugs-r20-opus 1: a private repo on an http:// git host got no credential
// line (only https did), so its clone failed.
func TestR20GitCredentialsForHTTPHost(t *testing.T) {
	e := newEnv(t, proto.GuestConfig{Repos: []proto.Repo{{URL: "http://git.local:3000/team/app.git", Token: "tok123"}}})
	e.a.writeGitCredentials()
	b, err := os.ReadFile(e.a.gitCredFile())
	if err != nil || !strings.Contains(string(b), "http://x-access-token:tok123@git.local:3000") {
		t.Fatalf("credentials %q, %v", b, err)
	}
}
