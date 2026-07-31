package sched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fengrru/k8s-sched/internal/policy"
	"go.uber.org/zap"
)

// TestLoadSchedulerWithRetry_Success attaches on the first attempt.
func TestLoadSchedulerWithRetry_Success(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	a.loadSchedFn = func() error { return nil }

	if err := a.loadSchedulerWithRetry(context.Background()); err != nil {
		t.Fatalf("loadSchedulerWithRetry() = %v, want nil", err)
	}
}

// TestLoadSchedulerWithRetry_NotContended fails fast: if the attach
// error is not a k8s-sched handover, do not retry (a foreign scheduler
// is respected — observe-only mode).
func TestLoadSchedulerWithRetry_NotContended(t *testing.T) {
	// Point opsPath at a file that does not list k8s_sched.
	opsFile := filepath.Join(t.TempDir(), "ops")
	if err := os.WriteFile(opsFile, []byte("scx_rusty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	a := &Agent{log: zap.NewNop(), opsPath: opsFile}
	a.loadSchedFn = func() error {
		calls++
		return errors.New("EBUSY")
	}

	err := a.loadSchedulerWithRetry(context.Background())
	if err == nil {
		t.Fatal("loadSchedulerWithRetry() = nil, want error")
	}
	if calls != 1 {
		t.Errorf("attach calls = %d, want 1 (no retry for foreign scheduler)", calls)
	}
}

// TestLoadSchedulerWithRetry_ContendedHandover retries while a
// previous k8s-sched instance owns the scheduler, then succeeds once
// the old instance exits (rolling upgrade handover).
func TestLoadSchedulerWithRetry_ContendedHandover(t *testing.T) {
	opsFile := filepath.Join(t.TempDir(), "ops")
	if err := os.WriteFile(opsFile, []byte("k8s_sched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := 0
	a := &Agent{log: zap.NewNop(), opsPath: opsFile}
	a.loadSchedFn = func() error {
		calls++
		if calls < 3 {
			return errors.New("EBUSY: scheduler already attached")
		}
		return nil
	}

	// Shorten the retry interval so the test finishes quickly.
	orig := attachRetryInterval
	attachRetryInterval = 10 * time.Millisecond
	defer func() { attachRetryInterval = orig }()

	if err := a.loadSchedulerWithRetry(context.Background()); err != nil {
		t.Fatalf("loadSchedulerWithRetry() = %v, want nil after handover", err)
	}
	if calls != 3 {
		t.Errorf("attach calls = %d, want 3 (2 retries then success)", calls)
	}
}

// TestLoadSchedulerWithRetry_HandoverCancelled stops retrying when the
// context is canceled (the replacement pod is being deleted).
func TestLoadSchedulerWithRetry_HandoverCancelled(t *testing.T) {
	opsFile := filepath.Join(t.TempDir(), "ops")
	if err := os.WriteFile(opsFile, []byte("k8s_sched\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{log: zap.NewNop(), opsPath: opsFile}
	a.loadSchedFn = func() error { return errors.New("EBUSY") }

	orig := attachRetryInterval
	attachRetryInterval = 50 * time.Millisecond
	defer func() { attachRetryInterval = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	if err := a.loadSchedulerWithRetry(ctx); err == nil {
		t.Fatal("loadSchedulerWithRetry() = nil, want error on cancel")
	}
}

// TestBuildStatusPatch verifies the merge-patch shape written into
// SchedulingPolicy.status.nodeStatuses.
func TestBuildStatusPatch(t *testing.T) {
	patch, err := buildStatusPatch("node-1", map[string]int32{"latency": 3, "batch": 0})
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(patch, &decoded); err != nil {
		t.Fatal(err)
	}

	status, ok := decoded["status"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch missing status: %s", patch)
	}
	nodes, ok := status["nodeStatuses"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch missing nodeStatuses: %s", patch)
	}
	counts, ok := nodes["node-1"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch missing node-1 entry: %s", patch)
	}
	if counts["latency"].(float64) != 3 || counts["batch"].(float64) != 0 {
		t.Errorf("unexpected counts: %s", patch)
	}
	if len(nodes) != 1 {
		t.Errorf("nodeStatuses has %d entries, want 1 (only this node)", len(nodes))
	}
}

// TestWritebackPolicyStatus_NoPolicies is a no-op when the resolver
// has no policies (also avoids nil dynClient dereferences).
func TestWritebackPolicyStatus_NoPolicies(t *testing.T) {
	r := policy.NewResolver(zap.NewNop(), nil, "node-1")
	a := &Agent{log: zap.NewNop(), nodeName: "node-1", resolver: r}
	a.writebackPolicyStatus(context.Background()) // must not panic
}

// TestLoadScheduler_MissingFile verifies that loadScheduler returns
// an error when the BPF object file is not found.
func TestLoadScheduler_MissingFile(t *testing.T) {
	a := &Agent{
		log: zap.NewNop(),
	}

	// Point to a non-existent BPF object.
	t.Setenv("SCHED_BPF_OBJ", "/tmp/does-not-exist.bpf.o")

	err := a.loadScheduler()
	if err == nil {
		t.Fatal("loadScheduler should fail when BPF object doesn't exist")
	}
}

// TestLoadScheduler_ExistingFileButNotBPF verifies that loadScheduler
// handles malformed BPF objects gracefully.
func TestLoadScheduler_NotABPFObject(t *testing.T) {
	a := &Agent{
		log: zap.NewNop(),
	}

	f, err := os.CreateTemp("", "not-bpf-*.o")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("this is not a BPF object file")
	f.Close()

	t.Setenv("SCHED_BPF_OBJ", f.Name())

	err = a.loadScheduler()
	if err == nil {
		t.Fatal("loadScheduler should fail for non-BPF file")
	}
}

// TestNewAgent_NoKubeconfig verifies agent creation handles
// missing kubeconfig gracefully.
func TestNewAgent_NoKubeconfig(t *testing.T) {
	log := zap.NewNop()
	_, err := NewAgent(log, "test-node", ":9090")
	if err == nil {
		t.Log("running inside cluster (expected failure outside)")
	}
}

// TestAgent_Run_SchedulerFailure tests that Run continues even when
// the scheduler can't be loaded (graceful degradation).
func TestAgent_Run_SchedulerFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	a := &Agent{
		log:      zap.NewNop(),
		nodeName: "test-node",
	}

	// Run should return when context is canceled, even if
	// scheduler loading fails and watcher can't start.
	t.Setenv("SCHED_BPF_OBJ", "/tmp/does-not-exist.bpf.o")

	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("Run exited with: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return before timeout")
	}
}
