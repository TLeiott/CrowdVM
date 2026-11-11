package server

import "github.com/crowdvm/crowdvm/internal/proto"

// CILabelOK exposes the runs-on label parser to the external tests.
func CILabelOK(l string) bool { _, err := parseCILabel(l); return err == nil }

// GCNow runs the deletion operations and the snapshot GC once (tests).
func (s *Server) GCNow() {
	s.processDeletionOps(s.ctx)
	s.gcSnapshots(s.ctx)
}

// SweepOrphanBlobsNow erases orphan snapshot blobs of any age (tests).
func (s *Server) SweepOrphanBlobsNow() {
	old := orphanBlobAge
	orphanBlobAge = 0
	defer func() { orphanBlobAge = old }()
	s.sweepOrphanBlobs()
}

// ReconcileVMNow handles one VM report from node nodeID (tests).
func (s *Server) ReconcileVMNow(nodeID string, st proto.VMStatus) { s.reconcileVM(nodeID, st, true) }

// OTelPruneCPU seeds an OTel sampler with CPU samples of ids, prunes it
// and returns how many remain (tests).
func OTelPruneCPU(s *Server, ids []string) int {
	o := &otelState{s: s, cpu: map[string]otelCPU{}}
	for _, id := range ids {
		o.cpu[id] = otelCPU{}
	}
	o.pruneCPU()
	return len(o.cpu)
}
