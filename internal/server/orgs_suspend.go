package server

// Organization suspension (Boat 403 org_suspended, "the organization wallet
// is suspended"). An admin suspends an organization's wallet: creates,
// resumes, forks and CI jobs billed to it get 403 org_suspended, and its
// running sandboxes are stopped (unless stopSandboxes is false). Reads,
// stops and deletes keep working. Unsuspend lifts it.

import (
	"context"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("org_suspend", store.Migration{Version: 1, SQL: `
ALTER TABLE orgs ADD COLUMN suspended_at INTEGER;
ALTER TABLE orgs ADD COLUMN suspended_reason TEXT;
`})
	RegisterFeature("org_suspend", func(s *Server) error {
		s.API("POST /admin/orgs/{org}/suspend", ActionAdmin, s.handleOrgSuspend)
		s.API("POST /admin/orgs/{org}/unsuspend", ActionAdmin, s.handleOrgUnsuspend)
		return nil
	})
}

// orgSuspension returns when and why org is suspended (0 = not suspended).
func (s *Server) orgSuspension(orgID string) (int64, string) {
	var at *int64
	var why *string
	_ = s.DB.QueryRow(`SELECT suspended_at, suspended_reason FROM orgs WHERE id=?`, orgID).Scan(&at, &why)
	if at == nil {
		return 0, ""
	}
	r := ""
	if why != nil {
		r = *why
	}
	return *at, r
}

// checkOrgActive refuses starting compute billed to a suspended org.
func (s *Server) checkOrgActive(org *Org) error {
	if org == nil {
		return nil
	}
	at, why := s.orgSuspension(org.ID)
	if at == 0 {
		return nil
	}
	msg := "The organization " + org.Name + " is suspended by an administrator, so it cannot start sandboxes."
	if why != "" {
		msg += " Reason: " + why
	}
	return ErrD(403, "org_suspended", msg, map[string]any{"org": org.ID, "suspendedAt": tsMs(at), "reason": nz(why)})
}

// adminOrg resolves {org} (id or name) for admin routes.
func (s *Server) adminOrg(c *Ctx) (*Org, error) {
	ref := c.Param("org")
	if o, err := s.OrgByID(ref); err == nil {
		return o, nil
	}
	o, err := scanOrg(s.DB.QueryRow(`SELECT `+orgCols+` FROM orgs o WHERE lower(o.name)=lower(?)`, ref))
	if err != nil {
		return nil, errNotFound("Organization")
	}
	return o, nil
}

func (s *Server) handleOrgSuspend(c *Ctx) error {
	var req struct {
		Reason        string  `json:"reason"`
		StopSandboxes OptBool `json:"stopSandboxes"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	o, err := s.adminOrg(c)
	if err != nil {
		return err
	}
	now := store.Now()
	if _, err := s.DB.Exec(`UPDATE orgs SET suspended_at=COALESCE(suspended_at,?), suspended_reason=? WHERE id=?`, now, nz(req.Reason), o.ID); err != nil {
		return err
	}
	stopped := []string{}
	if !req.StopSandboxes.Set || req.StopSandboxes.Value {
		rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE org_id=? AND deleted_at IS NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, o.ID)
		if err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					stopped = append(stopped, id)
				}
			}
			rows.Close()
		}
		s.Go(func(context.Context) {
			for _, id := range stopped {
				if sb, err := s.SandboxByID(id); err == nil && (sb.Active() || sb.State == api.StateProvisioning) {
					_, _ = s.requestStop(sb, false, "org_suspended")
				}
			}
		})
	}
	msg := "An administrator suspended the organization " + o.Name + ": it cannot start sandboxes until it is unsuspended."
	if req.Reason != "" {
		msg += " Reason: " + req.Reason
	}
	s.walletNotify(o.ID, "", "org_suspended", "Organization suspended", msg)
	s.audit(c, "org.suspend", o.ID, map[string]any{"reason": req.Reason, "stopped": len(stopped)})
	at, why := s.orgSuspension(o.ID)
	return c.OK(200, "org.suspended", map[string]any{"org": map[string]any{"id": o.ID, "name": o.Name, "suspendedAt": tsMs(at), "suspendedReason": nz(why)},
		"stoppedSandboxes": stopped})
}

func (s *Server) handleOrgUnsuspend(c *Ctx) error {
	o, err := s.adminOrg(c)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`UPDATE orgs SET suspended_at=NULL, suspended_reason=NULL WHERE id=?`, o.ID); err != nil {
		return err
	}
	s.walletNotify(o.ID, "", "org_unsuspended", "Organization active again", "The organization "+o.Name+" can start sandboxes again.")
	s.audit(c, "org.unsuspend", o.ID, nil)
	return c.OK(200, "org.unsuspended", map[string]any{"org": map[string]any{"id": o.ID, "name": o.Name, "suspendedAt": nil, "suspendedReason": nil}})
}
