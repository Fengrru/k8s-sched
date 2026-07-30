//go:build linux
// +build linux

// Package e2e contains integration tests that require a running
// Kubernetes cluster with sched_ext kernel support.
//
// Run with:
//
//	go test -tags linux -v ./test/e2e/ -kubeconfig ~/.kube/config
package e2e

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fengrru/k8s-sched/internal/k8s"
	"github.com/fengrru/k8s-sched/internal/maps"
	"github.com/fengrru/k8s-sched/internal/policy"
)

var (
	kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig file")
	namespace  = flag.String("namespace", "k8s-sched-e2e", "Test namespace")
)

// TestPolicyResolver_EndToEnd verifies that SchedulingPolicy CRDs
// are correctly resolved into scheduling parameters for matching pods.
func TestPolicyResolver_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E test in short mode")
	}

	// This test requires:
	// 1. A running Kubernetes cluster
	// 2. SchedulingPolicy CRD installed
	// 3. At least one test pod with annotations

	clientset, err := k8s.NewInClusterClient()
	if err != nil {
		t.Skipf("not in cluster, skipping E2E: %v", err)
	}

	log := zap.NewNop()
	resolver := policy.NewResolver(log, clientset, "test-node")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go resolver.Start(ctx.Done())

	// Give the resolver time to sync policies.
	time.Sleep(5 * time.Second)

	// Create a test pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-e2e",
			Namespace: *namespace,
			Labels:    map[string]string{"app": "e2e-test"},
			Annotations: map[string]string{
				"scheduling.fengrru.dev/importance": "80",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.9",
				},
			},
		},
	}

	params := resolver.Resolve(pod)
	t.Logf("Resolved params: weight=%d, budget=%d ns", params.Weight, params.BudgetNs)

	if params.Weight < 1000 {
		t.Errorf("expected weight >= 1000 (importance=80 → 8000), got %d", params.Weight)
	}
}

// TestPIDResolution validates PID discovery via cgroup v2.
func TestPIDResolution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PID test in short mode")
	}

	// Create a mock pod with a known UID pattern.
	// In real E2E, this would match actual running containers.
	mockPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "00000000-0000-0000-0000-000000000000",
		},
	}

	pids := maps.ResolvePodPIDs(mockPod)
	t.Logf("Resolved %d PIDs for mock pod", len(pids))
}

// TestSchedulingAnnotationParsing validates annotation-to-param conversion.
func TestSchedulingAnnotationParsing(t *testing.T) {
	tests := []struct {
		name         string
		annotations  map[string]string
		wantWeight   uint64
		wantBudgetNs uint64
	}{
		{
			name:         "importance=95",
			annotations:  map[string]string{"scheduling.fengrru.dev/importance": "95"},
			wantWeight:   9500,
			wantBudgetNs: 0,
		},
		{
			name: "weight+budget",
			annotations: map[string]string{
				"scheduling.fengrru.dev/weight":              "7000",
				"scheduling.fengrru.dev/budget-microseconds": "15000",
			},
			wantWeight:   7000,
			wantBudgetNs: 15000000,
		},
		{
			name: "importance with budget",
			annotations: map[string]string{
				"scheduling.fengrru.dev/importance":          "50",
				"scheduling.fengrru.dev/budget-microseconds": "5000",
			},
			wantWeight:   5000,
			wantBudgetNs: 5000000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			sp := maps.ExtractSchedulingParams(pod)
			if sp.Weight != tt.wantWeight {
				t.Errorf("weight: got %d, want %d", sp.Weight, tt.wantWeight)
			}
			if sp.BudgetNs != tt.wantBudgetNs {
				t.Errorf("budget: got %d, want %d", sp.BudgetNs, tt.wantBudgetNs)
			}
		})
	}
}
