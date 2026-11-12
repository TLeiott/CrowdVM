//go:build !linux && !windows

package bench

func cpuTimes() (idle, total uint64, ok bool) { return 0, 0, false }

func pressure() (cpu, mem *float64) { return nil, nil }
