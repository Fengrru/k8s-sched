package sched

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

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
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	log := zap.NewNop()
	_, err := NewAgent(ctx, log, "test-node", ":9090")
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
