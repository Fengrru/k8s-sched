# Contributing to k8s-sched

Thank you for your interest in contributing to k8s-sched! This document provides guidelines and instructions for contributing.

## Development Setup

### Prerequisites

- Go 1.25+
- Linux kernel 6.12+ with `CONFIG_SCHED_CLASS_EXT=y` (for integration tests)
- clang >= 16, libbpf >= 1.2 (for BPF compilation)
- controller-gen (for CRD generation)
- golangci-lint (for linting)

### Getting Started

```bash
# Clone the repository
git clone https://github.com/fengrru/k8s-sched.git
cd k8s-sched

# Install dependencies
go mod download

# Build
make build

# Run tests
make test

# Run linter
make lint
```

## Making Changes

### Branch Naming

- `feat/<description>` - New features
- `fix/<description>` - Bug fixes
- `refactor/<description>` - Code refactoring
- `docs/<description>` - Documentation changes
- `ci/<description>` - CI/CD changes

### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `ci`, `chore`

Examples:
```
feat(policy): addCEL condition cache
fix(map): handle stale PID cleanup race
docs: update installation guide
```

### Code Style

- Use `gofmt` / `goimports` for formatting
- Follow [Effective Go](https://go.dev/doc/effective_go) conventions
- Add comments for exported functions and types
- Keep functions focused and small
- Write table-driven tests where appropriate

### Testing

```bash
# Run all unit tests
make test

# Run benchmarks
make bench

# Run tests requiring sched_ext kernel (Linux only)
make test-linux
```

Ensure all tests pass before submitting a PR. The CI will run:

1. `golangci-lint` for code quality
2. Unit tests with race detection
3. Build verification

### eBPF Development

If modifying the BPF scheduler (`bpf/k8s_sched.bpf.c`):

1. Regenerate the BPF object: `make generate-ebpf`
2. Test on a machine with `CONFIG_SCHED_CLASS_EXT=y`
3. Verify with `bpftool prog list` after loading

## Pull Request Process

1. Fork the repository and create your branch from `main`
2. Make your changes following the guidelines above
3. Add or update tests as needed
4. Ensure `make lint` and `make test` pass
5. Update documentation if your change affects the API or user-facing behavior
6. Submit your PR with a clear description of the changes and motivation

### PR Title

Use the same Conventional Commits format as commit messages.

### What We Look For

- Clear problem statement and solution description
- Appropriate test coverage
- No regressions in existing functionality
- Clean, readable code
- Documentation updates for user-facing changes

## Reporting Issues

### Bug Reports

Use the [bug report template](https://github.com/fengrru/k8s-sched/issues/new?template=bug_report.md). Include:

- Kubernetes version
- Kernel version (`uname -r`)
- `CONFIG_SCHED_CLASS_EXT` status
- Steps to reproduce
- Expected vs actual behavior

### Feature Requests

Use the [feature request template](https://github.com/fengrru/k8s-sched/issues/new?template=feature_request.md). Describe:

- The problem you're trying to solve
- Your proposed solution
- Alternatives you considered

## License

By contributing to k8s-sched, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
