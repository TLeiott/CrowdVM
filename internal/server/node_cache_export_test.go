package server

import (
	"context"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// EvictStoppedCopies runs the node cache eviction pass (tests).
func (s *Server) EvictStoppedCopies() { s.evictStoppedCopies(context.Background()) }

// ExpireBootGrants simulates the expiry of a boot's layer protection (tests).
func (s *Server) ExpireBootGrants(nodeID, head string) {
	for _, id := range s.snapshotChain(head) {
		blobGrants.Delete(nodeID + "/" + id)
	}
}

// AbortResumeWithOpenRun opens a billing run on id, then aborts its resume (tests).
func (s *Server) AbortResumeWithOpenRun(id, msg string) error {
	if _, err := s.DB.Exec(`UPDATE sandboxes SET state='provisioning' WHERE id=?`, id); err != nil {
		return err
	}
	s.ensureRun(id)
	sb, err := s.SandboxByID(id)
	if err != nil {
		return err
	}
	s.abortBoot(sb, proto.BootResume, "", msg)
	return nil
}
