#!/usr/bin/env bash
# vm-smoke.sh — runs INSIDE a virtme-ng VM booted on a sched_ext kernel.
#
# Usage (from repo root, on a Linux host with virtme-ng + qemu + clang):
#   go test -c -o bin/sched-smoke.test ./internal/sched/
#   vng --verbose --rw -r v6.14 -- bash hack/vm-smoke.sh
#
# Expects in the repo root:
#   bin/bpftool          static bpftool (github.com/libbpf/bpftool releases)
#   bin/sched-smoke.test prebuilt test binary (go test -c ./internal/sched/)
#
# Steps:
#   1. dump vmlinux.h from the *running* kernel's BTF
#   2. compile the BPF scheduler against it
#   3. load + attach the struct_ops and verify scheduling actually happens
set -euxo pipefail

# virtme-ng normally drops us in as root; re-exec via sudo if not.
if [ "$(id -u)" -ne 0 ]; then
	exec sudo -E bash "$0" "$@"
fi

uname -r
if [ ! -d /sys/kernel/sched_ext ]; then
	echo "ERROR: kernel lacks CONFIG_SCHED_CLASS_EXT (need 6.12+ with sched_ext)" >&2
	exit 1
fi

# 1. vmlinux.h from the exact kernel the scheduler will run on.
./bin/bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h

# 2. Compile BPF C -> .o (same invocation as `make generate-ebpf`).
clang -O2 -target bpf -g -I bpf -c bpf/k8s_sched.bpf.c -o bpf/k8s_sched.bpf.o

# 3. Load, attach, and verify the scheduler end to end.
mountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf
SCHED_SMOKE=1 SCHED_BPF_OBJ="$PWD/bpf/k8s_sched.bpf.o" \
	./bin/sched-smoke.test -test.v -test.run TestRealKernel

echo "smoke test passed on $(uname -r)"
