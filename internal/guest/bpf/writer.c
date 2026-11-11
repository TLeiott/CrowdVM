//go:build ignore

// SPDX-License-Identifier: GPL-2.0
// Process output tracer (CMD-021): write(2)/writev(2) to a tty, pipe or
// socket and send(2)/sendto(2)/sendmsg(2) to a socket, up to MAX_DATA bytes
// per call, into a ring buffer; the exit status when a process ends. Userspace
// (internal/guest/tracer_bpf.go) classifies each process once and stores
// its mode here (skip / tty only), so an unrelated process costs one hash
// lookup per write. A per-process token bucket (64 KiB/s, 1 MiB burst,
// EV_COST extra per write) bounds what a chatty process sends. Minimal
// CO-RE type definitions instead of vmlinux.h.

#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>

#define MAX_DATA 4096
#define MAX_IOV 8
#define RATE (64 << 10)
#define BURST (1 << 20)
#define EV_COST 128

#define S_IFMT 00170000
#define S_IFSOCK 0140000
#define S_IFCHR 0020000
#define S_IFIFO 0010000

enum { EV_WRITE = 0, EV_EXEC = 1, EV_EXIT = 2 };
enum { M_ALL = 0, M_SKIP = 1, M_TTY = 2 };
enum { FD_TTY = 1, FD_PIPE = 2, FD_SOCK = 3 };
#define F_DROPPED 1 // writes of this process were dropped since its last event
#define F_SEND 2    // send*(2): one message (e.g. a syslog datagram)

struct inode {
	unsigned short i_mode;
	__u32 i_rdev;
} __attribute__((preserve_access_index));
struct file {
	struct inode *f_inode;
} __attribute__((preserve_access_index));
struct fdtable {
	unsigned int max_fds;
	struct file **fd;
} __attribute__((preserve_access_index));
struct files_struct {
	struct fdtable *fdt;
} __attribute__((preserve_access_index));
struct mm_struct {
	unsigned long arg_start;
	unsigned long arg_end;
} __attribute__((preserve_access_index));
struct signal_struct {
	struct {
		int counter;
	} live; // threads not yet exiting
} __attribute__((preserve_access_index));
struct task_struct {
	int tgid;
	int exit_code;
	struct task_struct *real_parent;
	struct files_struct *files;
	struct mm_struct *mm;
	struct signal_struct *signal;
} __attribute__((preserve_access_index));

struct iovec {
	void *iov_base;
	__u64 iov_len;
};

// The start of the user ABI struct msghdr (64-bit).
struct user_msghdr {
	void *msg_name;
	__u64 msg_namelen;
	const struct iovec *msg_iov;
	__u64 msg_iovlen;
};

// tracepoint/syscalls/sys_enter_*: fd, then buf/vec/msg, count/vlen/len.
struct sys_enter_ctx {
	__u64 common;
	__s32 nr;
	__u32 pad;
	__u64 fd;
	__u64 buf;
	__u64 count;
};

struct event {
	__u32 tgid;
	__u32 ppid;
	__u32 fd; // EV_EXIT: the exit status (wait(2) format)
	__u32 len;
	__u32 kind;
	__u32 flags; // F_DROPPED | fd type << 8
	__u64 cgroup;
	char comm[16];
	__u8 data[2 * MAX_DATA]; // EV_WRITE: up to MAX_DATA bytes written; EV_EXEC: argv
				 // (twice the size, so the verifier accepts the iovec copy)
};

struct state {
	__u64 last;
	__s64 tokens;
	__u32 mode;
	__u32 dropped;
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 4 << 20);
} events SEC(".maps");

// tgid -> state, created on the first wanted write. Userspace only updates
// existing entries (BPF_EXIST), so a verdict never outlives its process.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 32768);
	__type(key, __u32);
	__type(value, struct state);
} procs SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct event);
} scratch SEC(".maps");

// fd_type: FD_TTY (char major 4, 5, 136-143), FD_PIPE, FD_SOCK or 0.
static __always_inline __u32 fd_type(struct task_struct *t, __u32 fd)
{
	struct fdtable *fdt = BPF_CORE_READ(t, files, fdt);
	if (!fdt || fd >= BPF_CORE_READ(fdt, max_fds))
		return 0;
	struct file **fds = BPF_CORE_READ(fdt, fd);
	struct file *f = 0;
	if (bpf_probe_read_kernel(&f, sizeof(f), &fds[fd]) || !f)
		return 0;
	struct inode *ino = BPF_CORE_READ(f, f_inode);
	switch (BPF_CORE_READ(ino, i_mode) & S_IFMT) {
	case S_IFIFO:
		return FD_PIPE;
	case S_IFSOCK:
		return FD_SOCK;
	case S_IFCHR: {
		__u32 major = BPF_CORE_READ(ino, i_rdev) >> 20;
		return major == 4 || major == 5 || (major >= 136 && major <= 143) ? FD_TTY : 0;
	}
	}
	return 0;
}

static __always_inline struct event *header(struct task_struct *t, __u32 tgid, __u32 kind)
{
	__u32 zero = 0;
	struct event *e = bpf_map_lookup_elem(&scratch, &zero);
	if (!e)
		return 0;
	e->tgid = tgid;
	e->ppid = BPF_CORE_READ(t, real_parent, tgid);
	e->kind = kind;
	e->fd = 0;
	e->len = 0;
	e->flags = 0;
	e->cgroup = bpf_get_current_cgroup_id();
	bpf_get_current_comm(e->comm, sizeof(e->comm));
	return e;
}

// take charges the token bucket; 0 when the write is dropped.
static __always_inline int take(struct state *s, __u64 n)
{
	__u64 now = bpf_ktime_get_ns(), dt = now - s->last;
	if (dt > 20000000000ULL)
		dt = 20000000000ULL;
	__s64 tok = s->tokens + (__s64)(dt * RATE / 1000000000ULL);
	if (tok > BURST)
		tok = BURST;
	s->last = now;
	__s64 cost = (n > MAX_DATA ? MAX_DATA : n) + EV_COST;
	if (tok < cost) {
		s->tokens = tok;
		s->dropped = 1;
		return 0;
	}
	s->tokens = tok - cost;
	return 1;
}

static __always_inline void emit(struct state *s, struct event *e, __u32 fd, __u32 type, __u32 flag, __u32 n)
{
	e->fd = fd;
	e->len = n;
	e->flags = (s->dropped ? F_DROPPED : 0) | flag | type << 8;
	s->dropped = 0;
	bpf_ringbuf_output(&events, e, __builtin_offsetof(struct event, data) + n, 0);
}

// send records one buffer (write, sendto).
static __always_inline void send(struct task_struct *t, struct state *s, __u32 tgid, __u32 fd, __u32 type,
				 __u32 flag, const void *buf, __u64 count)
{
	if (!take(s, count))
		return;
	struct event *e = header(t, tgid, EV_WRITE);
	if (!e)
		return;
	__u32 n = count > MAX_DATA ? MAX_DATA : count;
	if (bpf_probe_read_user(e->data, n, buf))
		return;
	emit(s, e, fd, type, flag, n);
}

// send_iov records the first MAX_IOV buffers of an iovec array (writev,
// sendmsg) as one event, so a message split into header and body stays one.
static __always_inline void send_iov(struct task_struct *t, struct state *s, __u32 tgid, __u32 fd, __u32 type,
				     __u32 flag, const struct iovec *vec, __u64 vlen)
{
	struct event *e = header(t, tgid, EV_WRITE);
	if (!e)
		return;
	__u64 off = 0;
	for (int i = 0; i < MAX_IOV && i < vlen && off < MAX_DATA; i++) {
		struct iovec iov;
		if (bpf_probe_read_user(&iov, sizeof(iov), &vec[i]))
			break;
		__u64 n = iov.iov_len > MAX_DATA - off ? MAX_DATA - off : iov.iov_len;
		// bounds the verifier sees (clang would fold them away)
		barrier_var(off);
		barrier_var(n);
		if (off >= MAX_DATA || n > MAX_DATA)
			break;
		if (n && bpf_probe_read_user(e->data + off, n, iov.iov_base))
			break;
		off += n;
	}
	barrier_var(off);
	if (off && off <= MAX_DATA && take(s, off))
		emit(s, e, fd, type, flag, off);
}

// wanted returns the process state for a write worth sending (0: drop it).
static __always_inline struct state *wanted(struct task_struct *t, __u32 tgid, __u64 fd, __u64 count, __u32 *type)
{
	if (count == 0 || fd > 0xffff)
		return 0;
	struct state *s = bpf_map_lookup_elem(&procs, &tgid);
	if (s && s->mode == M_SKIP)
		return 0;
	*type = fd_type(t, fd);
	if (!*type || (s && s->mode == M_TTY && *type != FD_TTY))
		return 0;
	if (s)
		return s;
	struct state init = {.last = bpf_ktime_get_ns(), .tokens = BURST};
	bpf_map_update_elem(&procs, &tgid, &init, BPF_NOEXIST);
	return bpf_map_lookup_elem(&procs, &tgid);
}

SEC("tracepoint/syscalls/sys_enter_write")
int trace_write(struct sys_enter_ctx *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	__u32 tgid = bpf_get_current_pid_tgid() >> 32, type = 0;
	struct state *s = wanted(t, tgid, ctx->fd, ctx->count, &type);
	if (s)
		send(t, s, tgid, ctx->fd, type, 0, (const void *)ctx->buf, ctx->count);
	return 0;
}

SEC("tracepoint/syscalls/sys_enter_writev")
int trace_writev(struct sys_enter_ctx *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	__u32 tgid = bpf_get_current_pid_tgid() >> 32, type = 0;
	struct state *s = wanted(t, tgid, ctx->fd, ctx->count, &type);
	if (s)
		send_iov(t, s, tgid, ctx->fd, type, 0, (const void *)ctx->buf, ctx->count);
	return 0;
}

// send(2) is sendto(2) in glibc. send*(2) on anything but a socket fails, so
// fd_type is FD_SOCK here.
SEC("tracepoint/syscalls/sys_enter_sendto")
int trace_sendto(struct sys_enter_ctx *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	__u32 tgid = bpf_get_current_pid_tgid() >> 32, type = 0;
	struct state *s = wanted(t, tgid, ctx->fd, ctx->count, &type);
	if (s && type == FD_SOCK)
		send(t, s, tgid, ctx->fd, type, F_SEND, (const void *)ctx->buf, ctx->count);
	return 0;
}

// sendmsg(2): syslog(3), logger and sd_journal_* (the journal), most UDP.
SEC("tracepoint/syscalls/sys_enter_sendmsg")
int trace_sendmsg(struct sys_enter_ctx *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	__u32 tgid = bpf_get_current_pid_tgid() >> 32, type = 0;
	struct state *s = wanted(t, tgid, ctx->fd, 1, &type);
	if (!s || type != FD_SOCK)
		return 0;
	struct user_msghdr m;
	if (bpf_probe_read_user(&m, sizeof(m), (const void *)ctx->buf) || !m.msg_iovlen)
		return 0;
	send_iov(t, s, tgid, ctx->fd, type, F_SEND, m.msg_iov, m.msg_iovlen);
	return 0;
}

// exec: the program changed, so userspace classifies it again, with the
// new argv (read here, where it is certainly mapped).
SEC("tracepoint/sched/sched_process_exec")
int trace_exec(void *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	bpf_map_delete_elem(&procs, &tgid);
	struct event *e = header(t, tgid, EV_EXEC);
	if (!e)
		return 0;
	unsigned long start = BPF_CORE_READ(t, mm, arg_start), end = BPF_CORE_READ(t, mm, arg_end);
	__u32 n = end > start ? end - start : 0;
	if (n > MAX_DATA)
		n = MAX_DATA;
	if (n && bpf_probe_read_user(e->data, n, (const void *)start))
		n = 0;
	e->len = n;
	bpf_ringbuf_output(&events, e, __builtin_offsetof(struct event, data) + n, 0);
	return 0;
}

// exit of a process (its last thread; signal->live already counts this
// one out): userspace finishes its record with the exit status, which
// do_exit stored in exit_code before this tracepoint.
SEC("tracepoint/sched/sched_process_exit")
int trace_exit(void *ctx)
{
	struct task_struct *t = (void *)bpf_get_current_task();
	if (BPF_CORE_READ(t, signal, live.counter) != 0)
		return 0;
	__u32 tgid = bpf_get_current_pid_tgid() >> 32, dropped = 0;
	struct state *s = bpf_map_lookup_elem(&procs, &tgid);
	if (s) {
		dropped = s->dropped;
		bpf_map_delete_elem(&procs, &tgid);
	}
	struct event *e = header(t, tgid, EV_EXIT);
	if (!e)
		return 0;
	e->fd = BPF_CORE_READ(t, exit_code);
	e->flags = dropped ? F_DROPPED : 0;
	bpf_ringbuf_output(&events, e, __builtin_offsetof(struct event, data), 0);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
