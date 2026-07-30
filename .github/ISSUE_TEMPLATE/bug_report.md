---
name: Bug Report
about: Report a bug in k8s-sched
title: "[BUG] "
labels: bug
assignees: ''
---

## Environment

- **k8s-sched version**:
- **Kubernetes version**: (e.g. 1.28.2)
- **Kernel version**: (e.g. 6.12-generic) — run `uname -r`
- **CONFIG_SCHED_CLASS_EXT**: (yes/no/unknown) — run `zgrep CONFIG_SCHED_CLASS_EXT /proc/config.gz`
- **Node OS**: (e.g. Ubuntu 22.04)

## Description

A clear description of the bug.

## Steps to Reproduce

1. ...
2. ...
3. ...

## Expected Behavior

What you expected to happen.

## Actual Behavior

What actually happened.

## Logs

<details>
<summary>Agent logs</summary>

```
(paste logs here)
```

</details>

<details>
<summary>BPF verifier output (if applicable)</summary>

```
(paste bpftool prog list output here)
```

</details>

## Additional Context

Any other relevant information (Helm values, Pod annotations, SchedulingPolicy CRDs, etc.).
