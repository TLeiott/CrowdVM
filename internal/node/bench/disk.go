package bench

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"github.com/crowdvm/crowdvm/internal/proto"
)

const diskBlock = 4 << 20

// Disk reports free space in dir and sequential throughput: write total bytes
// in 4 MiB blocks + fsync, then read them back bypassing the page cache
// (posix_fadvise DONTNEED on Linux, FILE_FLAG_NO_BUFFERING on Windows).
func Disk(ctx context.Context, dir string, total int64) (proto.DiskBench, error) {
	r := proto.DiskBench{Path: dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, err
	}
	free, err := diskFree(dir)
	if err != nil {
		return r, err
	}
	r.FreeGiB = float64(free) / (1 << 30)
	// Never use more than 1/4 of the free space.
	total = min(total, int64(free/4)/diskBlock*diskBlock)
	if total < diskBlock {
		return r, fmt.Errorf("not enough free space for the disk test")
	}

	f, err := os.CreateTemp(dir, ".crowdvm-bench-*")
	if err != nil {
		return r, err
	}
	path := f.Name()
	defer os.Remove(path)

	buf := alignedBlock()
	_, _ = rand.Read(buf) // incompressible
	start := time.Now()
	var n int64
	for n < total && ctx.Err() == nil {
		w, err := f.Write(buf)
		n += int64(w)
		if err != nil {
			f.Close()
			return r, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return r, err
	}
	r.WriteMBps = float64(n) / time.Since(start).Seconds() / 1e6
	f.Close()

	rf, err := openUncached(path)
	if err != nil {
		return r, err
	}
	defer rf.Close()
	start = time.Now()
	var rn int64
	for ctx.Err() == nil {
		m, err := rf.Read(buf)
		rn += int64(m)
		if err == io.EOF {
			break
		}
		if err != nil {
			return r, err
		}
	}
	r.ReadMBps = float64(rn) / time.Since(start).Seconds() / 1e6
	return r, nil
}

// alignedBlock returns a diskBlock-sized buffer aligned to 4 KiB (needed for
// unbuffered I/O on Windows).
func alignedBlock() []byte {
	b := make([]byte, diskBlock+4096)
	off := int(4096 - uintptr(unsafe.Pointer(&b[0]))&4095)
	if off == 4096 {
		off = 0
	}
	return b[off : off+diskBlock]
}

// DataDirFree is a helper for capacity defaults.
func DataDirFree(dir string) (uint64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	return diskFree(filepath.Clean(dir))
}
