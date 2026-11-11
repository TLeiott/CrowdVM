package server_test

import (
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"testing"
)

func TestBughuntR3SolOrgKeyCannotAccessOtherOrg(t *testing.T) {
	env := servertest.New(t)
	user, _ := env.NewUser("orgowner", false)
	session := env.Session(user)
	orgs := make([]string, 2)
	for i, name := range []string{"Alpha", "Beta"} {
		r := env.Do(session, "POST", "/api/v1/orgs", map[string]any{"name": name})
		if r.Status != 201 {
			t.Fatal(r)
		}
		orgs[i] = r.Str("org.id")
	}
	r := env.Do(session, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "alpha only", "preset": "admin", "org": orgs[0]})
	if r.Status != 201 {
		t.Fatal(r)
	}
	key := r.Str("secret")
	for _, method := range []string{"GET", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			var body any
			if method == "PATCH" {
				body = map[string]any{"name": "Changed by wrong org key"}
			}
			r := env.Do(key, method, "/api/v1/orgs/"+orgs[1], body)
			if r.Status/100 == 2 {
				t.Fatalf("Alpha key accessed Beta with %s: %s", method, r)
			}
		})
	}
}
