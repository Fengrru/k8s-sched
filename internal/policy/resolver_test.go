package policy

import (
	"testing"

	"github.com/fengrru/k8s-sched/api/v1alpha1"
	"github.com/fengrru/k8s-sched/internal/maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseAnnotationWeight(t *testing.T) {
	tests := []struct {
		name string
		ann  map[string]string
		want uint64
	}{
		{"empty", nil, 0},
		{"weight-9000", map[string]string{"scheduling.fengrru.dev/weight": "9000"}, 9000},
		{"weight-out-of-range", map[string]string{"scheduling.fengrru.dev/weight": "10001"}, 0},
		{"importance-95", map[string]string{"scheduling.fengrru.dev/importance": "95"}, 9500},
		{"importance-1", map[string]string{"scheduling.fengrru.dev/importance": "1"}, 100},
		{"weight-overrides-importance", map[string]string{
			"scheduling.fengrru.dev/weight":     "2000",
			"scheduling.fengrru.dev/importance": "95",
		}, 2000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maps.ParseAnnotationWeight(tt.ann)
			if got != tt.want {
				t.Errorf("ParseAnnotationWeight() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseAnnotationBudget(t *testing.T) {
	tests := []struct {
		name   string
		ann    map[string]string
		wantNs uint64
	}{
		{"empty", nil, 0},
		{"20ms", map[string]string{"scheduling.fengrru.dev/budget-microseconds": "20000"}, 20000000},
		{"zero", map[string]string{"scheduling.fengrru.dev/budget-microseconds": "0"}, 0},
		{"invalid", map[string]string{"scheduling.fengrru.dev/budget-microseconds": "abc"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maps.ParseAnnotationBudget(tt.ann)
			if got != tt.wantNs {
				t.Errorf("ParseAnnotationBudget() = %d, want %d", got, tt.wantNs)
			}
		})
	}
}

func TestPodSelectorMatches(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	tests := []struct {
		name     string
		policy   *v1alpha1.SchedulingPolicy
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "nil selector matches all",
			policy: &v1alpha1.SchedulingPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec:       v1alpha1.SchedulingPolicySpec{},
			},
			pod:      &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}}},
			expected: true,
		},
		{
			name: "matching label",
			policy: &v1alpha1.SchedulingPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.SchedulingPolicySpec{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "payment"},
					},
				},
			},
			pod:      &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "payment"}}},
			expected: true,
		},
		{
			name: "non-matching label",
			policy: &v1alpha1.SchedulingPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "test"},
				Spec: v1alpha1.SchedulingPolicySpec{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "payment"},
					},
				},
			},
			pod:      &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "batch"}}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.podSelectorMatches(tt.policy, tt.pod)
			if got != tt.expected {
				t.Errorf("podSelectorMatches() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestResolveAnnotationsOnly(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"scheduling.fengrru.dev/weight":              "8000",
				"scheduling.fengrru.dev/budget-microseconds": "30000",
			},
		},
	}

	params := r.Resolve(pod)
	if params.Weight != 8000 {
		t.Errorf("Resolve() weight = %d, want 8000", params.Weight)
	}
	if params.BudgetNs != 30000000 {
		t.Errorf("Resolve() budget = %d, want 30000000", params.BudgetNs)
	}
}

func TestResolveDefaultParams(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
	}

	params := r.Resolve(pod)
	if params.Weight != 1000 {
		t.Errorf("Resolve() default weight = %d, want 1000", params.Weight)
	}
	if params.BudgetNs != 0 {
		t.Errorf("Resolve() default budget = %d, want 0", params.BudgetNs)
	}
}

func TestCELConditionEvaluation(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	policy := &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "conditional-policy"},
		Spec: v1alpha1.SchedulingPolicySpec{
			CELCondition: "signal.podName == 'test-pod'",
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
	}

	if !r.celConditionPasses(policy, pod) {
		t.Error("CEL condition 'signal.podName == \"test-pod\"' should pass")
	}

	pod.Name = "other-pod"
	if r.celConditionPasses(policy, pod) {
		t.Error("CEL condition should fail for non-matching pod name")
	}
}

func TestPolicyNames(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")
	r.mu.Lock()
	r.policies["test-policy"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy"},
	}
	r.mu.Unlock()

	names := r.PolicyNames()
	if len(names) != 1 || names[0] != "test-policy" {
		t.Errorf("PolicyNames() = %v, want [test-policy]", names)
	}
}

func TestResolve_HighestWeightWins(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	// Register two policies matching the same pod.
	r.mu.Lock()
	r.policies["low-priority"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "low-priority"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "shared"},
			},
			Weight: 3000,
		},
	}
	r.policies["high-priority"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "high-priority"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "shared"},
			},
			Weight: 7000,
		},
	}
	r.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "shared"},
		},
	}

	params := r.Resolve(pod)
	if params.Weight != 7000 {
		t.Errorf("Resolve() weight = %d, want 7000 (highest among matches)", params.Weight)
	}
}

func TestResolve_MaxBudget(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	// Two policies: one sets small budget, one sets larger budget.
	r.mu.Lock()
	r.policies["small-budget"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "small-budget"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector:        &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Weight:             5000,
			BudgetMicroseconds: 15000,
		},
	}
	r.policies["large-budget"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "large-budget"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector:        &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Weight:             8000,
			BudgetMicroseconds: 30000,
		},
	}
	r.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "test"},
		},
	}

	params := r.Resolve(pod)
	if params.Weight != 8000 {
		t.Errorf("Resolve() weight = %d, want 8000 (max)", params.Weight)
	}
	if params.BudgetNs != 30000000 {
		t.Errorf("Resolve() budget = %d, want 30000000 (max budget)", params.BudgetNs)
	}
}

func TestResolve_AnnotationOverridesCRD(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	r.mu.Lock()
	r.policies["crd-policy"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "crd-policy"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector:        &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Weight:             3000,
			BudgetMicroseconds: 10000,
		},
	}
	r.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "test"},
			Annotations: map[string]string{
				"scheduling.fengrru.dev/weight":              "9500",
				"scheduling.fengrru.dev/budget-microseconds": "50000",
			},
		},
	}

	params := r.Resolve(pod)
	// Annotations should override CRD values.
	if params.Weight != 9500 {
		t.Errorf("Resolve() weight = %d, want 9500 (annotation overrides CRD)", params.Weight)
	}
	if params.BudgetNs != 50000000 {
		t.Errorf("Resolve() budget = %d, want 50000000 (annotation overrides CRD)", params.BudgetNs)
	}
}

func TestResolve_CELConditionFilters(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	r.mu.Lock()
	r.policies["conditional"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "conditional"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Weight:       9000,
			CELCondition: "signal.podName == 'special-pod'",
		},
	}
	r.mu.Unlock()

	// Pod that doesn't match CEL condition.
	ordinaryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ordinary-pod",
			Labels: map[string]string{"app": "test"},
		},
	}
	params := r.Resolve(ordinaryPod)
	if params.Weight != 1000 {
		t.Errorf("Resolve() weight = %d, want 1000 (CEL condition filtered out policy)", params.Weight)
	}

	// Pod that matches CEL condition.
	specialPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "special-pod",
			Labels: map[string]string{"app": "test"},
		},
	}
	params = r.Resolve(specialPod)
	if params.Weight != 9000 {
		t.Errorf("Resolve() weight = %d, want 9000 (CEL condition passed)", params.Weight)
	}
}

func TestResolve_NoPoliciesNoAnnotations(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"app": "unmatched"},
		},
	}

	params := r.Resolve(pod)
	if params.Weight != 1000 {
		t.Errorf("Resolve() weight = %d, want 1000 (default)", params.Weight)
	}
	if params.BudgetNs != 0 {
		t.Errorf("Resolve() budget = %d, want 0 (default)", params.BudgetNs)
	}
}

func TestActivePodCount(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	if count := r.ActivePodCount("nonexistent"); count != 0 {
		t.Errorf("ActivePodCount for missing policy = %d, want 0", count)
	}

	r.mu.Lock()
	r.policies["existing"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "existing"},
	}
	r.mu.Unlock()

	// No pods resolved yet.
	if count := r.ActivePodCount("existing"); count != 0 {
		t.Errorf("ActivePodCount for existing policy without matches = %d, want 0", count)
	}

	// Resolve a matching pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
			UID:  "aaaa-bbbb-cccc",
		},
	}
	r.Resolve(pod)
	if count := r.ActivePodCount("existing"); count != 1 {
		t.Errorf("ActivePodCount after resolving pod = %d, want 1", count)
	}
}
