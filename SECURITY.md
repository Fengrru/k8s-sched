# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in k8s-sched, please report it responsibly.

**Do NOT open a public GitHub issue for security vulnerabilities.**

Instead, please email: **fengrru@users.noreply.github.com**

Include the following in your report:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

## Response Timeline

- **Acknowledgment**: Within 48 hours
- **Initial assessment**: Within 1 week
- **Fix or mitigation**: Depends on severity, typically within 2 weeks for critical issues

## Scope

This security policy applies to:

- The k8s-sched Go agent (`cmd/agent`)
- The eBPF scheduler (`bpf/k8s_sched.bpf.c`)
- Helm chart and deployment manifests

### Out of Scope

- Upstream kernel sched_ext vulnerabilities (report to Linux kernel security)
- Kubernetes API server security (report to Kubernetes security)
- Third-party dependencies (report to upstream)

## Security Considerations

k8s-sched runs with elevated privileges by design (requires `BPF`, `SYS_ADMIN`, `PERFMON` capabilities). The agent:

- Loads BPF programs into the kernel
- Reads `/proc` for PID resolution
- Accesses the Kubernetes API for Pod and CRD watching

Ensure your cluster follows Kubernetes security best practices:

- Use RBAC to limit access to the agent's ServiceAccount
- Run the DaemonSet with `hostPID: true` only on trusted nodes
- Monitor the agent's logs and metrics for anomalies

## Supported Versions

| Version | Supported          |
|---------|-------------------|
| 0.1.x   | Yes (active development) |
