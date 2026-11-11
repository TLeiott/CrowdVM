// Package bpf holds the guest's eBPF output tracer (writer.c), compiled
// with bpf2go. The generated Go and .o files are checked in, so a normal
// build needs no clang; regenerate with `go generate ./internal/guest/bpf`
// (needs clang, llvm-strip and the libbpf headers).
package bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel -output-stem writer Writer writer.c -- -O2 -Wall
