package server

import "context"

// EvictStoppedCopies runs the node cache eviction pass (tests).
func (s *Server) EvictStoppedCopies() { s.evictStoppedCopies(context.Background()) }

// ExpireBootGrants simulates the expiry of a boot's layer protection (tests).
func (s *Server) ExpireBootGrants(nodeID, head string) {
	for _, id := range s.snapshotChain(head) {
		blobGrants.Delete(nodeID + "/" + id)
	}
}
