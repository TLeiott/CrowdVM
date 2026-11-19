package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

// Issue #9: large balances printed as 1.00001e+06.
func TestFmtCredits(t *testing.T) {
	for in, want := range map[any]string{1000010.0: "1000010", 42.5: "42.5", 0.1 + 0.2: "0.3", 0.00012: "0.0001", 1e9: "1000000000", -3.0: "-3", "n/a": "n/a"} {
		if got := fmtCredits(in); got != want {
			t.Errorf("fmtCredits(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCreditsPrintWithoutExponent(t *testing.T) {
	e := newEnv(t)
	e.f.On("GET /api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "credits.info", map[string]any{"wallet": map[string]any{"balanceCredits": 1000010.0, "enforced": false}, "earnedCredits": 2e6, "spentCredits": 1})
	})
	for _, args := range [][]string{{"credits"}, {"status"}} {
		r := e.mustRun(append([]string{"--json=false"}, args...)...)
		if strings.Contains(r.out, "e+") || !strings.Contains(r.out, "1000010") {
			t.Errorf("%v: %q", args, r.out)
		}
	}
}
