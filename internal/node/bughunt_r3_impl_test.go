package node

import (
	"runtime"
	"strconv"
	"testing"
)

func TestR3ConfigRejectsUnusableValues(t *testing.T) {
	for _, kv := range [][2]string{{"cpus", strconv.Itoa(runtime.NumCPU() + 1)}, {"cpus", "-1"}, {"memory", "1K"}, {"memory", "NaN"}, {"disk", "+Inf"}, {"snapshot-interval", "1s"}, {"snapshot-interval", "-2s"}, {"direct-host", "notanip"}, {"ui-addr", ":99999"}, {"server", "notaurl"}, {"disk-overcommit", "NaN"}} {
		t.Run(kv[0]+kv[1], func(t *testing.T) {
			c := Config{}
			if err := c.Set(kv[0], kv[1]); err == nil {
				t.Fatal("accepted unusable value")
			}
		})
	}
}
