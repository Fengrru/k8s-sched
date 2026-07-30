<div align="center">

<img src="https://img.shields.io/badge/kernel-Linux%206.12%2B-blue" alt="Kernel">
<img src="https://img.shields.io/badge/Kubernetes-1.28%2B-326ce5" alt="K8s">
<img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License">
<img src="https://img.shields.io/badge/status-alpha-orange" alt="Status">

# k8s-sched

**Kubernetes-native CPU scheduler powered by sched_ext + eBPF**

*Give latency-critical workloads kernel-level priority. No sidecars, no hypervisors, no kernel patches.*

</div>

---

## What is this?

k8s-sched turns Pod annotations into **in-kernel CPU scheduling decisions**. A DaemonSet agent per node watches Pods, resolves them to host PIDs via cgroup v2, and writes `{weight, budget}` into a BPF map. An eBPF scheduler (`CONFIG_SCHED_CLASS_EXT=y`, Linux 6.12+) reads these maps and implements **weighted virtual-time dispatch with per-slice budget caps** — all inside the kernel, with no userspace round-trip on the hot path.

| Annotation | Effect |
|---|---|
| `scheduling.fengrru.dev/importance=95` | This Pod gets ~10x more CPU time than default |
| `scheduling.fengrru.dev/importance=10` | This Pod gets ~1/10th CPU time, yields to others |
| `scheduling.fengrru.dev/budget-microseconds=2000` | Preempt after 2ms per scheduling slice (caps the default 5ms slice) |

**For users:** add one annotation to your Deployment. The scheduler does the rest.

**For platform teams:** deploy one Helm chart. All nodes get the DaemonSet. No kernel recompile, no hypervisor, no sidecar injection — just `CONFIG_SCHED_CLASS_EXT=y` in your kernel.

## Why?

Kubernetes has `PriorityClass` for pod scheduling (deciding _which node_ to place a pod on). It has `requests/limits` for resource allocation. What it lacks is a way to say:

> "When this payment-processing pod and this batch-analytics pod are both running on the same node, I want the payment pod to get CPU cycles _first_, every time, without preempting or killing the batch pod."

This is the gap between **placement** and **runtime**. `k8s-sched` closes it.

### vs. existing approaches

| Approach | What it does | Limitation |
|---|---|---|
| `PriorityClass` | Schedules pods onto nodes | No runtime effect after placement |
| `CPU limits` | Caps CPU consumption | Doesn't express relative priority |
| `nice` / cgroup weight | Process-level priority | Not K8s-aware, no per-pod mapping |
| Cilium/Tetragon | Network + kernel enforcement | Doesn't implement a CPU scheduler |
| **k8s-sched** | **Kernel-level weighted scheduling per pod** | — |

## Architecture

```
                   ┌────────────────────┐
                   │    Kubernetes API   │
                   │  Pods + Scheduling  │
                   │  Policies (CRD)     │
                   └─────────┬──────────┘
                             │ watch
        ┌────────────────────┼────────────────────┐
        │                    ▼                     │
        │  ┌─────────────────────────────────┐    │
        │  │     Go Agent (DaemonSet)         │    │
        │  │                                 │    │
        │  │  Pod Informer ──→ Annotation Parser  │
        │  │                        │              │
        │  │              resolvePodPIDs()         │
        │  │              /proc/<pid>/cgroup       │
        │  │              grep "pod<UID>"          │
        │  │                        │              │
        │  │                        ▼              │
        │  │  ┌──────────────────────────────┐    │
        │  │  │ BPF map: task_params          │    │
        │  │  │ tgid → {weight, budget_ns}   │    │
        │  │  └──────────────┬───────────────┘    │
        │  └─────────────────┼────────────────────┘
        │                    │
        │  ┌─────────────────┼────────────────────┐
        │  │  BPF Scheduler (sched_ext ops)       │
        │  │                 │                     │
        │  │  select_cpu()   │  idle → local DSQ   │
        │  │  enqueue()      │  vtime insert       │
        │  │  dispatch()     │  K8S_DSQ → local    │
        │  │  stopping()     │  vtime accounting   │
        │  │                 │                     │
        │  │  CPU0  CPU1  CPU2  CPU3 ... CPU(N-1) │
        │  └──────────────────────────────────────┘
        │
        └── per Node ─────────────────────────────
```

### Scheduling algorithm

```
On stopping (task leaves CPU, charged by actual usage):
  vtime += (used_ns × 10000) / weight

  weight=10000 → charged 0.5ms per 5ms used → runs ~10x more
  weight=1000  → charged 5ms (default, 1:1)
  weight=100   → charged 50ms → runs ~1/10th

On enqueue (wakeup fairness clamp):
  vtime = max(vtime, vtime_now - DEFAULT_SLICE)
  → sleepers can't hoard credit and starve runners

Budget enforcement: slice = min(DEFAULT_SLICE, budget_ns)
  budget=2ms → kernel preempts after 2ms per slice
```

## Quick Start

```bash
# 1. Check kernel support
zgrep CONFIG_SCHED_CLASS_EXT /proc/config.gz

# 2. Install CRD + scheduler
kubectl apply -f config/crd/bases/scheduling.fengrru.dev_schedulingpolicies.yaml
helm install k8s-sched ./chart/k8s-sched \
  --namespace kube-system \
  --set image.repository=ghcr.io/fengrru/k8s-sched

# 3. Annotate your pods
kubectl annotate pod payment-api-7d4f8b9c-x2k3m \
  scheduling.fengrru.dev/importance=95
```

## CRD: SchedulingPolicy

```yaml
apiVersion: scheduling.fengrru.dev/v1alpha1
kind: SchedulingPolicy
metadata:
  name: latency-critical
spec:
  podSelector:
    matchLabels:
      app: payment-api
  weight: 9000             # 1-10000, higher = more CPU
  budgetMicroseconds: 2000 # 0 = unlimited; caps the 5ms default slice
```

| Field | Type | Default | Description |
|---|---|---|---|
| `podSelector` | LabelSelector | `{}` | Pods to apply this policy to |
| `weight` | int | 1000 | Scheduling weight (1–10000) |
| `budgetMicroseconds` | int | 0 | Max CPU per slice; 0 = no cap. Only values below the 5ms default slice have an effect |

## Pod Annotations

| Annotation | Values | Equivalent |
|---|---|---|
| `scheduling.fengrru.dev/importance` | 1–100 | `weight = importance × 100` |
| `scheduling.fengrru.dev/weight` | 1–10000 | Direct weight override |
| `scheduling.fengrru.dev/budget-microseconds` | µs | `budgetMicroseconds` |

`weight` overrides `importance` if both are set.

## Requirements

| Component | Minimum |
|---|---|
| Linux kernel | 6.12+ with `CONFIG_SCHED_CLASS_EXT=y` |
| Kubernetes | 1.28+ |
| Node capabilities | `BPF`, `SYS_ADMIN`, `PERFMON` |
| DaemonSet | `hostPID: true`, `privileged: true` |
| BPF toolchain | `clang >= 16`, `libbpf >= 1.2` (build only) |

## Performance

| Path | Latency | Overhead |
|---|---|---|
| enqueue (hot path) | in-kernel, ~200ns | negligible |
| dispatch | in-kernel, ~100ns | negligible |
| Pod → BPF map update | userspace, event-driven | <1ms |
| PID resolution | scan /proc on pod add | <50ms per pod |

The scheduling hot path runs entirely inside the kernel. Userspace is only involved when Pods are created, updated, or deleted.

## Development

```bash
# Prerequisites
apt install clang-17 libbpf-dev bpftool golang-go

# Build
make generate-ebpf   # BPF C → .o
make build           # Go binary → bin/agent

# Test
make test            # unit + vtime math
make test-linux      # integration (needs sched_ext kernel)

# Docker
docker build -t k8s-sched:latest .
```

## Test Coverage

| Package | Covers |
|---|---|
| `internal/maps/` | Annotation parsing, weight/budget extraction, vtime math |
| `internal/cel/` | CEL evaluation, LRU cache, concurrent access |
| `internal/k8s/` | Pod watcher lifecycle, field selector |
| `internal/sched/` | Scheduler loading, graceful failure |

## License

Apache 2.0 — see [LICENSE](LICENSE)

## Authors

**fengrru** — [github.com/fengrru](https://github.com/fengrru)

---

*Built on [sched_ext](https://github.com/sched-ext/scx) — the extensible BPF scheduler class backed by Meta and Google. Merged in Linux 6.12.*
