# Benchmark: does k8s-sched beat plain `cpu.weight`?

The core claim of k8s-sched is that in-kernel vtime scheduling improves
**tail latency for high-priority pods under CPU contention** beyond what
static cgroup `cpu.weight` can deliver. This document defines a
reproducible 3-arm experiment that either proves or falsifies that claim.

If arm C does not measurably beat arm B, the honest conclusion is that
the weighting feature duplicates `cpu.weight` and the project's value
must come from budget caps / strict priority tiers instead.

## Experiment design

One node, fully CPU-saturated. Two co-located workloads:

| Role | Workload | Why |
|---|---|---|
| Victim (latency-critical) | [`schbench`](https://kernel.googlesource.com/pub/scm/linux/kernel/git/mason/schbench) | Multithreaded message/worker model; reports scheduling **wakeup latency percentiles** directly. Being multithreaded, it also exercises the tgid-based param lookup. |
| Antagonist (batch) | `stress-ng --cpu $(nproc)` | Saturates every CPU with pure compute. |

Three arms, identical hardware/kernel/pods, only the priority mechanism changes:

| Arm | Mechanism | Setup |
|---|---|---|
| A | Baseline (CFS/EEVDF, no priorities) | k8s-sched not installed (or observe-only) |
| B | `cgroup v2 cpu.weight` | victim pod cgroup `cpu.weight=900`, antagonist `cpu.weight=20`; scheduler not loaded |
| C | k8s-sched | victim `weight=9000`, antagonist `weight=200`; identical 45:1 ratio as arm B |

Optional arm D (budget): arm C + `budget-microseconds=1000` on the
antagonist, measuring the effect of slice capping on victim p99.9.

## Environment requirements

- Single dedicated node, Linux 6.12+, `CONFIG_SCHED_CLASS_EXT=y`, cgroup v2
- CPU frequency governor pinned (`performance`), SMT state noted
- No other significant workloads on the node (cordon + dedicated namespace)
- Each arm: 5 runs x 120 s, discard first run (warm-up), report median of 4

## Manifests

Both pods must be **Burstable with identical requests and no CPU limits**
so that K8s-derived cgroup weights don't differ between arms:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: victim
  labels: { app: victim }
  # Arm C only:
  # annotations: { scheduling.fengrru.dev/weight: "9000" }
spec:
  nodeName: <bench-node>
  restartPolicy: Never
  containers:
    - name: schbench
      image: <schbench-image>   # build: gcc -O2 -pthread schbench.c -o schbench
      command: ["/schbench", "-m", "2", "-t", "8", "-r", "120"]
      resources:
        requests: { cpu: "1", memory: 256Mi }
---
apiVersion: v1
kind: Pod
metadata:
  name: antagonist
  labels: { app: antagonist }
  # Arm C only:
  # annotations: { scheduling.fengrru.dev/weight: "200" }
spec:
  nodeName: <bench-node>
  restartPolicy: Never
  containers:
    - name: stress
      image: ghcr.io/colinianking/stress-ng
      args: ["--cpu", "0", "--timeout", "150s", "--metrics-brief"]
      resources:
        requests: { cpu: "1", memory: 256Mi }
```

Arm B cgroup setup (run on the node after both pods are Running):

```bash
# Find each pod's cgroup and set weights to the same 45:1 ratio as arm C.
set_weight() {  # $1=pod-uid $2=weight
  d=$(find /sys/fs/cgroup/kubepods.slice -maxdepth 2 -name "*pod${1//-/_}*")
  echo "$2" > "$d/cpu.weight"
}
set_weight <victim-uid> 900
set_weight <antagonist-uid> 20
```

## Metrics to collect

| Metric | Source | Primary? |
|---|---|---|
| Victim wakeup latency p50 / p99 / p99.9 | schbench stdout | **yes** |
| Antagonist throughput (bogo ops/s) | stress-ng `--metrics-brief` | fairness cost |
| `k8s_sched_enqueues_total`, `k8s_sched_budget_capped_total` | agent `/metrics` (arm C/D) | sanity |
| `k8s_sched_params_mapped` = 2 | agent `/metrics` | sanity: both pods mapped |

Sanity checks before trusting a run (arm C):
`bpftool map dump pinned /sys/fs/bpf/k8s-sched/task_params` must contain
the tgids of both pods with the expected weights (equivalently:
`curl localhost:9090/debug/params` on the agent), and
`cat /sys/kernel/sched_ext/root/ops` must report `k8s_sched`.

## Success criteria

- **C vs B**: victim p99 improves by >= 20% with antagonist throughput
  degraded by <= 10% -> the vtime scheduler adds value beyond cpu.weight.
- **C ~= B** (within noise): weighting is a cpu.weight substitute;
  reposition the project around budget caps (arm D) and strict priority.
- **D vs C**: victim p99.9 improves when the antagonist is budget-capped
  at 1 ms -> the budget feature has independent, demonstrable value.

## Reporting

Publish per-arm raw schbench output plus a summary table
(arm x {p50, p99, p99.9, antagonist ops/s}) and the exact kernel version,
CPU model, SMT state, and image digests. A result that cannot be
reproduced from this file alone does not count.

## Quick benchmark (automated, inside a VM)

For a fast, reproducible EEVDF-vs-k8s-sched comparison without a
physical bench node, run `make vm-bench` (same prerequisites as
`make vm-smoke`): it boots a sched_ext kernel VM, compiles the BPF
scheduler against the running kernel, then runs the same schbench +
stress-ng workload twice — once on the kernel-default EEVDF and once
with k8s-sched attached:

```bash
make vm-bench            # or: vng --rw -r v6.14 -- bash hack/vm-bench.sh
```

The script prints percentile tables side by side. Caveats: a VM is not
noisy-neighbor-free (the host shares the CPU), and `-m 4 -t 8` is
smaller than the full experiment above. Treat VM numbers as
*sanity* results — publish the bare-metal experiment for real claims,
but always include the VM run plus kernel version in any report.

Tuning:

```bash
SCHED_BENCH_ARGS="-m 2 -t 8 -r 30" STRESS_ARGS="-c 2" make vm-bench
```
