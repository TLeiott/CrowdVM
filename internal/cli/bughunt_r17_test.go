package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Two ssh connections to two sandboxes start `crowdvm tunnel --pin` (the
// ssh-config ProxyCommand since r16) at the same moment, and so do two
// `crowdvm ssh` calls. Each one rewrites the shared known_hosts with a plain
// read-modify-write. The slower writer's file does not hold the other's line,
// and the host key check (StrictHostKeyChecking yes) then fails for it.
func TestBughuntR17KnownHostsConcurrentPinsKeepEveryLine(t *testing.T) {
	const hosts = 12
	for round := 0; round < 40; round++ {
		file := filepath.Join(t.TempDir(), "known_hosts")
		var wg sync.WaitGroup
		for i := 0; i < hosts; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_ = setKnownHost(file, fmt.Sprintf("bx_host%02d", i), fmt.Sprintf("ssh-ed25519 AAAAkey%02d", i))
			}(i)
		}
		wg.Wait()
		b, _ := os.ReadFile(file)
		for i := 0; i < hosts; i++ {
			if !strings.Contains(string(b), fmt.Sprintf("bx_host%02d ssh-ed25519 AAAAkey%02d\n", i, i)) {
				t.Fatalf("round %d: the pin of bx_host%02d is missing after %d concurrent pins:\n%s", round, i, hosts, b)
			}
		}
	}
}
