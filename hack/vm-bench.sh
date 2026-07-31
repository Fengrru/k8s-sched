#!/usr/bin/env bash
# vm-bench.sh — runs INSIDE a virtme-ng VM booted on a sched_ext kernel
# and benchmarks the kernel-default EEVDF scheduler against k8s-sched
# on the same kernel, same VM, same workload.
#
# Usage (from repo root, on a Linux host with virtme-ng + qemu + clang):
#   go test -c -o bin/sched-smoke.test ./internal/sched/
#   vng --verbose --rw -r v6.14 -- bash hack/vm-bench.sh
#
# Expects in the repo root:
#   bin/bpftool          static bpftool (github.com/libbpf/bpftool releases)
#   bin/sched-smoke.test prebuilt test binary (go test -c ./internal/sched/)
#
# Benchmarks (schbench + stress-ng, installed inside the VM when
# missing): p50/p90/p99 wakeup latency under load. Results are printed
# as a side-by-side table; publish them in docs/benchmark.md with the
# kernel, CPU count, and workload details.
set -euo pipefail

# virtme-ng normally drops us in as root; re-exec via sudo if not.
if [ "$(id -u)" -ne 0 ]; then
	exec sudo -E bash "$0" "$@"
fi

uname -r
if [ ! -d /sys/kernel/sched_ext ]; then
	echo "ERROR: kernel lacks CONFIG_SCHED_CLASS_EXT (need 6.12+ with sched_ext)" >&2
	exit 1
fi

# 1. Tooling: schbench (wakeup latency) + stress-ng (CPU contention).
apt-get update -qq >/dev/null 2>&1 || true
apt-get install -y -qq schbench stress-ng >/dev/null 2>&1 || \
	echo "WARNING: benchmark tools unavailable (schbench/stress-ng), results will be empty"

# 2. vmlinux.h from the exact kernel, then compile the BPF scheduler.
#    -DSCX_KERNEL_MAJOR/MINOR select the sched_ext kfunc naming for this
#    kernel: pre-6.13 uses scx_bpf_dispatch()/dispatch_vtime()/consume(),
#    6.13+ uses the renamed scx_bpf_dsq_insert()/insert_vtime()/move_to_local().
./bin/bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h
KMAJOR="$(uname -r | cut -d. -f1)"
KMINOR="$(uname -r | cut -d. -f2)"
clang -O2 -target bpf -g -DSCX_KERNEL_MAJOR="$KMAJOR" -DSCX_KERNEL_MINOR="$KMINOR" \
	-I bpf -I include -c bpf/k8s_sched.bpf.c -o bpf/k8s_sched.bpf.o

# 3. Workload: 4 worker groups x 8 threads, 15s, under 4 stress-ng CPU
# burners. Tune SCHED_BENCH_ARGS / STRESS_ARGS via env.
BENCH_ARGS="${SCHED_BENCH_ARGS:--m 4 -t 8 -r 15}"
STRESS_ARGS="${STRESS_ARGS:--c 4}"
NCPU="$(nproc)"

run_bench() {
	local tag="$1"
	shift
	# Kill leftovers, wait for the stress load to ramp.
	pkill -f stress-ng >/dev/null 2>&1 || true
	sleep 1
	stress-ng $STRESS_ARGS >/dev/null 2>&1 &
	local stress_pid=$!
	sleep 2
	echo "--- $tag ($(uname -r), $NCPU CPUs) ---"
	if command -v schbench >/dev/null; then
		schbench $BENCH_ARGS 2>&1 || true
	fi
	kill $stress_pid >/dev/null 2>&1 || true
	wait $stress_pid >/dev/null 2>&1 || true
}

# 4. Baseline: kernel default EEVDF (no scheduler attached).
run_bench "BASELINE EEVDF"

# 5. Load + attach k8s-sched, keep it running for the benchmark.
mountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf
SCHED_SMOKE=1 SCHED_BPF_OBJ="$PWD/bpf/k8s_sched.bpf.o" \
	./bin/sched-smoke.test -test.v -test.run TestRealKernel
cat /sys/kernel/sched_ext/root/ops

# 6. k8s-sched under the same workload.
run_bench "k8s-sched"

echo
echo "Done. Compare the p50/p90/p99 percentiles above; document the"
echo "numbers in docs/benchmark.md with kernel + workload details."
