# Stage 1: Compile BPF C → .o
# bpf/vmlinux.h must exist in the build context. Generate it on a
# BTF-enabled host first: make generate-vmlinux
FROM alpine:3.21 AS bpf
RUN apk add --no-cache clang llvm libbpf-dev
WORKDIR /src
COPY bpf/ ./bpf/
RUN test -f bpf/vmlinux.h || \
    (echo "ERROR: bpf/vmlinux.h missing; run 'make generate-vmlinux' before docker build" && exit 1)
RUN clang -O2 -target bpf -g -I bpf -c bpf/k8s_sched.bpf.c -o /k8s_sched.bpf.o

# Stage 2: Build Go binary
FROM golang:1.25-alpine AS go
RUN apk add --no-cache git ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /agent ./cmd/agent

# Stage 3: Runtime (K8s securityContext overrides user at runtime)
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=bpf /k8s_sched.bpf.o /etc/k8s-sched/k8s_sched.bpf.o
COPY --from=go /agent /agent
ENTRYPOINT ["/agent"]
