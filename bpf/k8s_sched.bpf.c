/*
 * k8s_sched - Kubernetes-aware sched_ext scheduler
 *
 * Implements weighted virtual time scheduling with per-task CPU budgets.
 * A Go DaemonSet agent watches Pod annotations and writes per-pod
 * {weight, budget_ns} to the cgroup_params BPF map, keyed by the
 * cgroup ID (kernfs inode) of the pod's cgroup subtree. A secondary
 * task_params map keyed by tgid serves as a fallback when cgroup
 * resolution is unavailable in userspace.
 *
 * Keying by cgroup ID (instead of PID) means processes forked after
 * the agent last scanned the pod inherit their pod's parameters
 * automatically, and recycled PIDs can never pick up another pod's
 * parameters.
 *
 * Scheduling algorithm:
 *   - Custom user DSQ ("k8s_vtime") supports vtime ordering.
 *     Internal DSQs (GLOBAL/LOCAL) only support FIFO.
 *   - enqueue: insert into the custom DSQ ordered by the task's vtime
 *     (clamped so long-sleeping tasks cannot hoard the CPU).
 *     Lower vtime = higher effective priority.
 *   - stopping: charge the consumed slice to the task's vtime, scaled
 *     inversely by weight. This is what makes weights translate into
 *     long-term CPU shares: heavy tasks accrue vtime slowly, light
 *     tasks quickly.
 *   - Budget: slice = min(DEFAULT_SLICE_NS, budget_ns). Kernel preempts
 *     naturally when slice expires. No extra tick logic needed.
 *     Budgets >= the default slice have no effect (it is a cap).
 *   - select_cpu: default wake-affine; if an idle CPU is found the
 *     task is inserted directly into that CPU's local DSQ.
 *
 * Build:
 *   bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h
 *   clang -O2 -target bpf -g -c bpf/k8s_sched.bpf.c -o bpf/k8s_sched.bpf.o
 */

#include <vmlinux.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "scx_common.bpf.h"

char _license[] SEC("license") = "GPL";

/* ---- BPF maps ---- */

struct task_params {
	u64 weight;
	u64 budget_ns;
};

/* Primary: keyed by cgroup ID (kernfs inode number) of any cgroup in
 * a pod's subtree. The agent writes one entry for the pod directory
 * and each descendant (container scopes), so every task in the pod
 * resolves here — including processes forked at any later time. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16384);
	__type(key, u64);
	__type(value, struct task_params);
} cgroup_params SEC(".maps");

/* Fallback: keyed by tgid (cgroup.procs lists thread-group leaders,
 * and every thread of a process shares its pod's parameters). Only
 * populated when the agent cannot resolve the pod's cgroup subtree
 * and had to fall back to /proc scanning. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, u32);
	__type(value, struct task_params);
} task_params SEC(".maps");

struct sched_stats {
	u64 enqueues;
	u64 local_dispatches; /* idle-CPU direct inserts from select_cpu */
	u64 budget_capped;
	u64 defaults;         /* enqueues of tasks without params */
};

/* Per-CPU: written on every enqueue, so avoid a shared cache line
 * bouncing between all cores. Userspace sums across CPUs. */
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, u32);
	__type(value, struct sched_stats);
} stats SEC(".maps");

/* ---- Constants ---- */

#define MAX_PRIO_WEIGHT    10000
#define DEFAULT_WEIGHT     1000
#define DEFAULT_SLICE_NS   5000000ULL   /* 5ms */
#define K8S_DSQ_ID         0x1000       /* Custom user DSQ ID */

/*
 * Global virtual clock. Advanced in .running to the vtime of the task
 * about to run; used in .enqueue to clamp the vtime of tasks that
 * slept for a long time so their backlog of "unused" vtime does not
 * let them monopolize the CPU after waking up.
 */
static u64 vtime_now;

/* ---- Helpers ---- */

static inline bool vtime_before(u64 a, u64 b)
{
	return (s64)(a - b) < 0;
}

static inline struct sched_stats *get_stats(void)
{
	u32 key = 0;

	return bpf_map_lookup_elem(&stats, &key);
}

/*
 * Cgroup ID of the task, for cgroup_params lookups. Callable from
 * rq-locked ops.* callbacks on their task argument (same pattern as
 * scx_flatcg). Returns 0 when the cgroup cannot be determined.
 */
static inline u64 task_cgroup_id(struct task_struct *p)
{
	struct cgroup *cgrp = scx_bpf_task_cgroup(p);
	u64 cgid = 0;

	if (cgrp) {
		cgid = cgrp->kn->id;
		bpf_cgroup_release(cgrp);
	}
	return cgid;
}

/*
 * Resolve a task's scheduling parameters. Explicit per-process
 * entries (fallback path) win over cgroup-level entries. Returns
 * true when parameters were found, false when defaults apply.
 * Stats are NOT touched here: callers that represent one scheduling
 * decision (enqueue/select_cpu) count exactly once.
 */
static inline bool lookup_task_params(struct task_struct *p,
				      struct task_params *tp)
{
	/* In-kernel p->pid is the thread ID; tgid is the process-level
	 * PID that userspace writes, shared by all threads. */
	u32 tgid = (u32)p->tgid;
	struct task_params *t;
	u64 cgid;

	t = bpf_map_lookup_elem(&task_params, &tgid);
	if (t) {
		*tp = *t;
		return true;
	}

	cgid = task_cgroup_id(p);
	if (cgid) {
		t = bpf_map_lookup_elem(&cgroup_params, &cgid);
		if (t) {
			*tp = *t;
			return true;
		}
	}

	tp->weight = DEFAULT_WEIGHT;
	tp->budget_ns = 0;
	return false;
}

/*
 * Budget caps the slice: slice = min(DEFAULT_SLICE_NS, budget_ns).
 * A budget can only shorten the slice (tail-latency control), never
 * extend it; budgets >= DEFAULT_SLICE_NS are effectively no-ops.
 */
static inline u64 task_slice(const struct task_params *tp)
{
	if (tp->budget_ns && tp->budget_ns < DEFAULT_SLICE_NS)
		return tp->budget_ns;
	return DEFAULT_SLICE_NS;
}

/* ---- sched_ext ops ---- */

s32 BPF_STRUCT_OPS(k8s_select_cpu, struct task_struct *p,
		   s32 prev_cpu, u64 wake_flags)
{
	bool is_idle = false;
	s32 cpu = scx_bpf_select_cpu_dfl(p, prev_cpu, wake_flags, &is_idle);

	if (is_idle) {
		/*
		 * An idle CPU was found: skip the vtime queue and run
		 * immediately on that CPU's local DSQ. There is no
		 * contention to arbitrate, so vtime ordering is moot.
		 */
		struct task_params tp;
		u64 slice;

		lookup_task_params(p, &tp);
		slice = task_slice(&tp);
		scx_dsq_insert(p, SCX_DSQ_LOCAL, slice, 0);

		struct sched_stats *s = get_stats();
		if (s) {
			s->local_dispatches++;
			if (slice < DEFAULT_SLICE_NS)
				s->budget_capped++;
		}
	}
	return cpu;
}

void BPF_STRUCT_OPS(k8s_enqueue, struct task_struct *p, u64 enq_flags)
{
	struct task_params tp;
	bool found = lookup_task_params(p, &tp);
	u64 slice = task_slice(&tp);
	u64 vtime = p->scx.dsq_vtime;

	/*
	 * Clamp: a task that slept a long time has a stale (small)
	 * vtime. Limit how far behind the global clock it may be to
	 * one default slice, mirroring scx_simple. Without this, a
	 * task idle for hours would starve everyone on wakeup.
	 */
	if (vtime_before(vtime, vtime_now - DEFAULT_SLICE_NS))
		vtime = vtime_now - DEFAULT_SLICE_NS;

	scx_dsq_insert_vtime(p, K8S_DSQ_ID, slice, vtime, enq_flags);

	struct sched_stats *s = get_stats();
	if (s) {
		s->enqueues++;
		/* Count only enqueues where the budget actually shortened
		 * the slice, i.e. the task will be preempted early. */
		if (slice < DEFAULT_SLICE_NS)
			s->budget_capped++;
		if (!found)
			s->defaults++;
	}
}

void BPF_STRUCT_OPS(k8s_dispatch, s32 cpu, struct task_struct *prev)
{
	/*
	 * Pull the lowest-vtime task from the custom DSQ into this
	 * CPU's local DSQ. All tasks pass through k8s_enqueue, so
	 * there is nothing to consume from the global DSQ.
	 */
	scx_dsq_move_to_local(K8S_DSQ_ID);
}

void BPF_STRUCT_OPS(k8s_running, struct task_struct *p)
{
	/*
	 * Advance the global vclock. Monotonicity is best-effort
	 * (racy across CPUs), which is fine for the wakeup clamp.
	 */
	if (vtime_before(vtime_now, p->scx.dsq_vtime))
		vtime_now = p->scx.dsq_vtime;
}

void BPF_STRUCT_OPS(k8s_stopping, struct task_struct *p, bool runnable)
{
	struct task_params tp;
	u64 slice, used;

	lookup_task_params(p, &tp);
	slice = task_slice(&tp);
	used = slice - p->scx.slice; /* consumed part of the slice */

	if (used > slice)
		used = slice; /* guard against underflow */

	/*
	 * Weighted virtual time:
	 *   vtime += (used * MAX_PRIO_WEIGHT) / weight
	 *
	 *   weight=10000 → +5ms per 5ms used → re-queues near the front
	 *   weight=1000  → +50ms (default)
	 *   weight=100   → +500ms → runs ~1/10th as often
	 *
	 * Charging on actual usage (not per enqueue) is what turns
	 * weights into long-term CPU shares.
	 * The divisor is clamped to 1 to avoid division by zero.
	 */
	p->scx.dsq_vtime +=
		(used * MAX_PRIO_WEIGHT) / (tp.weight ? tp.weight : 1);
}

void BPF_STRUCT_OPS(k8s_enable, struct task_struct *p)
{
	/* Start new tasks at the current global clock, not at zero. */
	p->scx.dsq_vtime = vtime_now;
}

s32 BPF_STRUCT_OPS_SLEEPABLE(k8s_init)
{
	/*
	 * Create a custom user DSQ that supports vtime (priority queue).
	 * Without this, weighted scheduling would silently degrade to FIFO.
	 */
	s32 ret = scx_bpf_create_dsq(K8S_DSQ_ID, -1 /* any NUMA node */);
	if (ret < 0)
		return ret;
	return 0;
}

void BPF_STRUCT_OPS(k8s_exit, struct scx_exit_info *ei)
{
	bpf_printk("k8s_sched exit: kind=%d", ei->kind);
}

SCX_OPS_DEFINE(k8s_sched,
	.select_cpu   = (void *)k8s_select_cpu,
	.enqueue      = (void *)k8s_enqueue,
	.dispatch     = (void *)k8s_dispatch,
	.running      = (void *)k8s_running,
	.stopping     = (void *)k8s_stopping,
	.enable       = (void *)k8s_enable,
	.init         = (void *)k8s_init,
	.exit         = (void *)k8s_exit,
	.flags        = 0,
	.name         = "k8s_sched",
	.timeout_ms   = 30000,
);
