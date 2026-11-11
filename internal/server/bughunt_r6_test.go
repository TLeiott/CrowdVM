package server_test

import (
	"fmt"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// Deleting an organization removes the org row but leaves everything keyed by
// its id (environments with their sealed secrets, git sources, policies,
// settings, ...) in the database for ever.
func TestBughuntR6OrgDeleteLeavesOrgDataBehind(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "r6gone"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	orgID := r.Str("org.id")
	if r := env.Do(key, "POST", "/api/v1/environments?org=r6gone", map[string]any{"name": "ci"}); r.Status/100 != 2 {
		t.Fatalf("create env: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r6gone", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	skip := map[string]bool{"credit_ledger": true, "audit_events": true, "wallets": true}
	rows, err := env.S.DB.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var left []string
	for _, tbl := range tables {
		if skip[tbl] || tbl == "orgs" {
			continue
		}
		cols, err := env.S.DB.Query(fmt.Sprintf(`SELECT name FROM pragma_table_info('%s') WHERE name IN ('owner_id','org_id')`, tbl))
		if err != nil {
			continue
		}
		var cn []string
		for cols.Next() {
			var c string
			cols.Scan(&c)
			cn = append(cn, c)
		}
		cols.Close()
		for _, c := range cn {
			var n int
			env.S.DB.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s=?`, tbl, c), orgID).Scan(&n)
			if n > 0 {
				left = append(left, fmt.Sprintf("%s.%s=%d", tbl, c, n))
			}
		}
	}
	if len(left) > 0 {
		t.Fatalf("rows still keyed by the deleted org %s: %v", orgID, left)
	}
}
