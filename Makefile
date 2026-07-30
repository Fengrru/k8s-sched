# k8s-sched
# Kubernetes-aware sched_ext CPU scheduler
#

# ---- Build targets ----
.PHONY: all build generate generate-ebpf generate-vmlinux manifests
.PHONY: test test-unit test-vtime test-all test-linux test-smoke vm-smoke bench
.PHONY: fmt vet lint clean docker-build help

all: generate build

# Generate eBPF Go stubs and CRD manifests
generate:
	$(MAKE) generate-ebpf
	$(MAKE) manifests

# Compile eBPF C to .o (requires bpf/vmlinux.h, see generate-vmlinux)
generate-ebpf:
	clang -O2 -target bpf -g -I bpf -I include -c bpf/k8s_sched.bpf.c -o bpf/k8s_sched.bpf.o

# Generate vmlinux.h from running kernel BTF.
# Required for BPF compilation on the target kernel.
# Alternative: download from https://github.com/sched-ext/scx/tree/main/scheds/include
generate-vmlinux:
	bpftool btf dump file /sys/kernel/btf/vmlinux format c > bpf/vmlinux.h

# Generate CRD manifests
manifests:
	controller-gen crd paths="./api/v1alpha1" output:crd:artifacts:config=config/crd/bases

# Compile Go binary
build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/agent ./cmd/agent

# ---- Test targets ----
test: test-unit test-vtime

test-unit:
	go test -race -v ./internal/cel/ ./internal/maps/ ./internal/k8s/ ./internal/policy/

test-vtime:
	go test -race -v -run TestVtime ./internal/maps/

test-all:
	go test -race -v ./...

# Run tests that require a sched_ext kernel
test-linux:
	go test -race -v -tags linux ./internal/sched/

# Real-kernel smoke test on the CURRENT kernel.
# Requires root + 6.12+ with CONFIG_SCHED_CLASS_EXT and a compiled
# bpf/k8s_sched.bpf.o (make generate-vmlinux generate-ebpf first).
test-smoke:
	SCHED_SMOKE=1 SCHED_BPF_OBJ=$(CURDIR)/bpf/k8s_sched.bpf.o \
		go test -v -count=1 -run TestRealKernel ./internal/sched/

# Full smoke test inside a sched_ext VM (no special host kernel needed).
# Requires: virtme-ng, qemu-system-x86, clang, and a static bpftool at
# bin/bpftool (https://github.com/libbpf/bpftool/releases).
# Same flow as the bpf-verify CI job.
vm-smoke:
	go test -c -o bin/sched-smoke.test ./internal/sched/
	vng --verbose --rw -r v6.14 -- bash hack/vm-smoke.sh

# Benchmark tests
bench:
	go test -bench=. -benchmem -run=^$$ ./internal/...

# ---- Quality ----
fmt:
	go fmt ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

# ---- Clean ----
clean:
	rm -rf bin/ bpf/*.o coverage.out coverage.html

# ---- Docker ----
docker-build:
	docker build -t ghcr.io/fengrru/k8s-sched:latest .

# ---- Help ----
help:
	@echo "Targets:"
	@echo "  all            - generate + build"
	@echo "  generate       - generate eBPF stubs + CRD manifests"
	@echo "  generate-ebpf  - compile BPF C -> .o"
	@echo "  generate-vmlinux - extract vmlinux.h from running kernel"
	@echo "  manifests      - generate CRD YAML"
	@echo "  build          - compile Go binary -> bin/agent"
	@echo ""
	@echo "  test           - run unit + vtime tests"
	@echo "  test-unit      - run unit tests"
	@echo "  test-vtime     - run vtime math tests"
	@echo "  test-all       - run all tests"
	@echo "  test-linux     - run tests requiring sched_ext kernel"
	@echo "  test-smoke     - real-kernel load+attach smoke test (root, sched_ext)"
	@echo "  vm-smoke       - smoke test inside a sched_ext VM via virtme-ng"
	@echo "  bench          - run benchmark tests"
	@echo ""
	@echo "  fmt            - format code"
	@echo "  vet            - run go vet"
	@echo "  lint           - run golangci-lint"
	@echo "  clean          - remove build artifacts"
	@echo "  docker-build   - build Docker image"
