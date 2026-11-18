package server

import "context"

// EvictStoppedCopies runs the node cache eviction pass (tests).
func (s *Server) EvictStoppedCopies() { s.evictStoppedCopies(context.Background()) }
