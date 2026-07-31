/*
 * scx_common.bpf.h - Minimal sched_ext BPF helpers
 *
 * sched_ext kfuncs were renamed in v6.13: scx_bpf_dispatch() →
 * scx_bpf_dsq_insert(), scx_bpf_dispatch_vtime() →
 * scx_bpf_dsq_insert_vtime(), scx_bpf_consume() →
 * scx_bpf_dsq_move_to_local(). Signatures and the allowed call
 * contexts are identical, so the only difference is the name emitted
 * at compile time.
 *
 * Build scripts pass -DSCX_KERNEL_MAJOR/-DSCX_KERNEL_MINOR derived from
 * the target kernel (e.g. `uname -r` inside the VM) to select the
 * matching naming; when unset, the 6.13+ names are assumed. Scheduler
 * code always calls the unified names defined below.
 *
 * In production, replace this with the full header from
 * https://github.com/sched-ext/scx/blob/main/scheds/include/scx/common.bpf.h
 * which also provides compat shims for older kernels.
 */

#ifndef __SCX_COMMON_BPF_H
#define __SCX_COMMON_BPF_H

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

/*
 * sched_ext kfunc naming per kernel generation (see header comment).
 * Legacy names apply to v6.12, the sched_ext release kernel.
 */
#if defined(SCX_KERNEL_MAJOR) && defined(SCX_KERNEL_MINOR)
#define __SCX_LEGACY_KFUNCS					\
	((SCX_KERNEL_MAJOR) < 6 ||				\
	 ((SCX_KERNEL_MAJOR) == 6 && (SCX_KERNEL_MINOR) < 13))
#else
#define __SCX_LEGACY_KFUNCS	0
#endif

#if __SCX_LEGACY_KFUNCS
#define scx_dsq_insert		scx_bpf_dispatch
#define scx_dsq_insert_vtime	scx_bpf_dispatch_vtime
#define scx_dsq_move_to_local	scx_bpf_consume
#else
#define scx_dsq_insert		scx_bpf_dsq_insert
#define scx_dsq_insert_vtime	scx_bpf_dsq_insert_vtime
#define scx_dsq_move_to_local	scx_bpf_dsq_move_to_local
#endif

/*
 * Dispatch Queue (DSQ) constants.
 *
 * These mirror enum scx_dsq_id_flags in the kernel (vmlinux.h). Built-in
 * DSQ IDs have the high bit set; user DSQs must keep it clear.
 */
#define SCX_DSQ_FLAG_BUILTIN	(1ULL << 63)
#define SCX_DSQ_INVALID		(SCX_DSQ_FLAG_BUILTIN | 0)
#define SCX_DSQ_GLOBAL		(SCX_DSQ_FLAG_BUILTIN | 1)
#define SCX_DSQ_LOCAL		(SCX_DSQ_FLAG_BUILTIN | 2)
#define SCX_DSQ_LOCAL_ON	(SCX_DSQ_FLAG_BUILTIN | 3)

#define SCX_SLICE_DFL		20000000ULL /* 20ms kernel default slice */

/* Enqueue flags (subset of enum scx_enq_flags) */
#define SCX_ENQ_WAKEUP		1ULL
#define SCX_ENQ_HEAD		(1ULL << 4)

/* Kick CPU flags (subset of enum scx_kick_flags) */
#define SCX_KICK_IDLE		1ULL
#define SCX_KICK_PREEMPT	(1ULL << 1)

/*
 * BPF kfunc declarations for sched_ext helpers. The names follow the
 * kernel generation selected above; call sites use the unified names
 * (scx_dsq_insert / scx_dsq_insert_vtime / scx_dsq_move_to_local).
 * Pre-merge RFC names (scx_bpf_dispatch_vtime, scx_bpf_consume, ...)
 * do not exist in released kernels other than the v6.12 variants below.
 */
extern s32 scx_bpf_create_dsq(u64 dsq_id, s32 node) __ksym;
extern void scx_bpf_destroy_dsq(u64 dsq_id) __ksym;
#if __SCX_LEGACY_KFUNCS
/* v6.12: pre-rename names. */
extern void scx_bpf_dispatch(struct task_struct *p, u64 dsq_id, u64 slice,
			     u64 enq_flags) __ksym;
extern void scx_bpf_dispatch_vtime(struct task_struct *p, u64 dsq_id,
				   u64 slice, u64 vtime,
				   u64 enq_flags) __ksym;
extern bool scx_bpf_consume(u64 dsq_id) __ksym;
#else
/* v6.13+: renamed kfuncs. */
extern void scx_bpf_dsq_insert(struct task_struct *p, u64 dsq_id, u64 slice,
			       u64 enq_flags) __ksym;
extern void scx_bpf_dsq_insert_vtime(struct task_struct *p, u64 dsq_id,
				     u64 slice, u64 vtime,
				     u64 enq_flags) __ksym;
extern bool scx_bpf_dsq_move_to_local(u64 dsq_id) __ksym;
#endif
extern void scx_bpf_kick_cpu(s32 cpu, u64 flags) __ksym;
extern s32 scx_bpf_select_cpu_dfl(struct task_struct *p, s32 prev_cpu,
				  u64 wake_flags, bool *is_idle) __ksym;

/*
 * Cgroup helpers. scx_bpf_task_cgroup() returns the (acquired) cgroup
 * of a task in an ops.* callback; requires CONFIG_EXT_GROUP_SCHED
 * (SCHED_CLASS_EXT + CGROUP_SCHED, both enabled on stock 6.12+ distro
 * kernels). The returned reference must be dropped with
 * bpf_cgroup_release().
 */
extern struct cgroup *scx_bpf_task_cgroup(struct task_struct *p) __ksym;
extern void bpf_cgroup_release(struct cgroup *cgrp) __ksym;

/*
 * Struct ops definition macros.
 *
 * BPF_STRUCT_OPS expands to a BPF_PROG in the right ELF section so the
 * callback keeps its named parameters (BPF_PROG unpacks the u64 context
 * array). The previous stub dropped the parameter list entirely, which
 * could never compile.
 */
#define BPF_STRUCT_OPS(name, args...)			\
	SEC("struct_ops/"#name)				\
	BPF_PROG(name, ##args)

#define BPF_STRUCT_OPS_SLEEPABLE(name, args...)		\
	SEC("struct_ops.s/"#name)			\
	BPF_PROG(name, ##args)

#define SCX_OPS_DEFINE(name, ...)			\
	SEC(".struct_ops.link")				\
	struct sched_ext_ops name = {			\
		__VA_ARGS__				\
	}

#endif /* __SCX_COMMON_BPF_H */
