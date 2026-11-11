//go:build !linux && !windows

package bench

import (
	"context"
	"os"

	"golang.org/x/sys/unix"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func cpuInfo(context.Context) (string, int) { return "unknown", 0 }
func memInfo() (uint64, uint64)             { return 0, 0 }

func diskFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func openUncached(path string) (*os.File, error) { return os.Open(path) }

func gpus(ctx context.Context) []proto.GPUInfo { return nvidiaSMI(ctx, "nvidia-smi") }

func virt(ctx context.Context) proto.VirtInfo {
	v := proto.VirtInfo{Accel: "none", QEMU: qemuVersion(ctx, "qemu-system-x86_64")}
	if v.QEMU != "" {
		v.Accel = "tcg"
	}
	return v
}
