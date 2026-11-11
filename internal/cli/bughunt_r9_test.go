package cli

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// bugs-r9-sonnet 4: cancelSignals leaves SIGHUP and SIGQUIT alone when they
// are inherited as ignored, but always registers SIGINT. A background job of
// a non-interactive shell (`crowdvm exec ID long-job &` in a script or CI
// step) starts with SIGINT and SIGQUIT ignored so that the script's Ctrl-C
// does not reach it; registering SIGINT turns the ignore off, so that Ctrl-C
// cancels the job and stops the remote command, the same effect the round-8
// nohup fix removed for SIGHUP.
func TestBughuntR9InheritedIgnoredSigintStaysIgnored(t *testing.T) {
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	for _, s := range cancelSignals() {
		if s == syscall.SIGINT {
			t.Fatal("SIGINT was inherited as ignored (shell background job), but cancelSignals registers it, which re-enables it")
		}
	}
}
