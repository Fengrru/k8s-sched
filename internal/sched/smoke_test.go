//go:build linux

package sched

import (
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/fengrru/k8s-sched/internal/maps"
)

// TestRealKernel_LoadAttachSchedule is the end-to-end smoke test that
// runs against a real sched_ext kernel. CI boots one in a VM via
// virtme-ng (see .github/workflows/ci.yml, hack/vm-smoke.sh); locally
// use `make test-smoke` on a 6.12+ kernel.
//
// It verifies the full scheduler lifecycle:
//  1. the BPF object loads and the struct_ops attaches
//  2. the kernel reports k8s_sched as the active root scheduler
//  3. pinned maps are writable from userspace and read back intact
//  4. the enqueue path actually runs (stats counters advance)
//
// Gated behind SCHED_SMOKE=1: requires root and CONFIG_SCHED_CLASS_EXT.
func TestRealKernel_LoadAttachSchedule(t *testing.T) {
	if os.Getenv("SCHED_SMOKE") == "" {
		t.Skip("set SCHED_SMOKE=1 to run (requires root + sched_ext kernel)")
	}

	log, _ := zap.NewDevelopment()
	a := &Agent{log: log, nodeName: "smoke"}

	if err := a.loadScheduler(); err != nil {
		t.Fatalf("load+attach failed: %v", err)
	}
	defer os.RemoveAll(mapPinDir)
	defer a.schedLink.Close()

	// The kernel exposes the active root scheduler in sysfs.
	ops, readErr := os.ReadFile("/sys/kernel/sched_ext/root/ops")
	if readErr != nil {
		t.Fatalf("read sched_ext sysfs (kernel without CONFIG_SCHED_CLASS_EXT?): %v", readErr)
	}
	if got := strings.TrimSpace(string(ops)); got != "k8s_sched" {
		t.Fatalf("active scheduler = %q, want k8s_sched", got)
	}

	// Userspace ↔ kernel map plumbing: write our own tgid, read it back.
	m := maps.New()
	if err := m.Open(); err != nil {
		t.Fatalf("open pinned maps: %v", err)
	}
	tgid := uint32(os.Getpid())
	want := maps.TaskParams{Weight: 5000, BudgetNs: 2_000_000}
	if err := m.TaskParams.Put(&tgid, &want); err != nil {
		t.Fatalf("write task_params: %v", err)
	}
	var got maps.TaskParams
	if err := m.TaskParams.Lookup(&tgid, &got); err != nil {
		t.Fatalf("read back task_params: %v", err)
	}
	if got != want {
		t.Fatalf("task_params round-trip = %+v, want %+v", got, want)
	}

	// Let the scheduler run for a bit, waking up periodically so this
	// process keeps getting enqueued, then check the counters moved.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stats, err := m.ReadStats()
	if err != nil {
		t.Fatalf("read stats: %v", err)
	}
	t.Logf("stats: enqueues=%d budget_capped=%d defaults=%d",
		stats.Enqueues, stats.BudgetCapped, stats.Defaults)
	if stats.Enqueues == 0 {
		t.Fatal("scheduler attached but enqueue counter never advanced")
	}
}
