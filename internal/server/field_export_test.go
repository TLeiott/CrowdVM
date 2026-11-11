package server

import "time"

// ForgetNodeConn drops node id's connection without the disconnect handling,
// like a node that went away while the server restarted (tests).
func (s *Server) ForgetNodeConn(id string) {
	if c := s.nodes.get(id); c != nil {
		s.nodes.remove(id, c)
	}
}

// SetNodeReconnectWindow sets how long nodes get to reconnect after a server
// start and returns the restore function (tests; call before servertest.New).
func SetNodeReconnectWindow(d time.Duration) func() {
	old := nodeReconnectWindow
	nodeReconnectWindow = d
	return func() { nodeReconnectWindow = old }
}

// RequestStop runs a server-initiated stop with reason (tests: billing,
// suspension and closure stops call requestStop like this).
func (s *Server) RequestStop(id, reason string) error {
	sb, err := s.SandboxByID(id)
	if err != nil {
		return err
	}
	_, err = s.requestStop(sb, false, reason)
	return err
}
