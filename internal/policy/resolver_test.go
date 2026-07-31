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

func TestResolve_PolicyDeprioritizes(t *testing.T) {
	// A policy weight below the 1000 default must lower the pod's
	// priority (not be swallowed by the max-with-default merge).
	r := NewResolver(nil, nil, "node-1")

	r.mu.Lock()
	r.policies["batch"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "batch"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "batch"}},
			Weight:      200,
		},
	}
	r.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"tier": "batch"}},
	}

	params := r.Resolve(pod)
	if params.Weight != 200 {
		t.Errorf("Resolve() weight = %d, want 200 (policy deprioritizes below default)", params.Weight)
	}
}

func TestResolve_PolicyZeroWeightKeepsDefault(t *testing.T) {
	// weight unset (0) means "no opinion": the default 1000 applies.
	r := NewResolver(nil, nil, "node-1")

	r.mu.Lock()
	r.policies["no-weight"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "no-weight"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
	}
	r.mu.Unlock()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}},
	}

	params := r.Resolve(pod)
	if params.Weight != 1000 {
		t.Errorf("Resolve() weight = %d, want 1000 (unset policy weight)", params.Weight)
	}
}

func TestCELCondition_DoubleContext(t *testing.T) {
	// weight/budgetUs are exposed as doubles; the documented example
	// mixes them with a fractional literal (CEL has no int*double
	// overload, so this would fail to compile for int64 vars).
	r := NewResolver(nil, nil, "node-1")

	policy := &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "budget-gated"},
		Spec: v1alpha1.SchedulingPolicySpec{
			CELCondition:       "context.budgetUs * 0.001 > 1.5",
			BudgetMicroseconds: 2000, // 2ms -> 2.0ms > 1.5ms
		},
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	if !r.celConditionPasses(policy, pod) {
		t.Error("CEL double arithmetic should pass: 2000 * 0.001 = 2.0 > 1.5")
	}

	policy.Spec.BudgetMicroseconds = 1000 // 1.0 > 1.5 -> false
	if r.celConditionPasses(policy, pod) {
		t.Error("CEL double arithmetic should fail: 1000 * 0.001 = 1.0 <= 1.5")
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

func TestResolve_QoSDefaults(t *testing.T) {
	// With no policy and no annotation, the pod's QoS class picks the
	// weight: guaranteed work keeps the default, best-effort work is
	// deprioritized. Zero-config mixed workloads.
	tests := []struct {
		name string
		qos  corev1.PodQOSClass
		want uint64
	}{
		{"guaranteed", corev1.PodQOSGuaranteed, 1000},
		{"burstable", corev1.PodQOSBurstable, 800},
		{"best-effort", corev1.PodQOSBestEffort, 200},
		{"unset", "", 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewResolver(nil, nil, "node-1")
			pod := &corev1.Pod{
				Status: corev1.PodStatus{QOSClass: tt.qos},
			}
			if got := r.Resolve(pod).Weight; got != tt.want {
				t.Errorf("Resolve() weight = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolve_QoSPolicyOverridesWeight(t *testing.T) {
	// Explicit policy weights beat the QoS default in both directions:
	// raising a best-effort pod, and deprioritizing a guaranteed one.
	r := NewResolver(nil, nil, "node-1")
	r.mu.Lock()
	r.policies["raise"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "raise"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
			Weight:      9000,
		},
	}
	r.policies["lower"] = &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "lower"},
		Spec: v1alpha1.SchedulingPolicySpec{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
			Weight:      300,
		},
	}
	r.mu.Unlock()

	raised := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u1", Labels: map[string]string{"app": "a"}},
		Status:     corev1.PodStatus{QOSClass: corev1.PodQOSBestEffort},
	}
	if got := r.Resolve(raised).Weight; got != 9000 {
		t.Errorf("best-effort raised by policy = %d, want 9000", got)
	}

	lowered := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "u2", Labels: map[string]string{"app": "b"}},
		Status:     corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
	}
	if got := r.Resolve(lowered).Weight; got != 300 {
		t.Errorf("guaranteed lowered by policy = %d, want 300", got)
	}
}

func TestCELCondition_ExtendedSignalVars(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	policy := &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-gated"},
		Spec: v1alpha1.SchedulingPolicySpec{
			CELCondition: "signal.podRestartCount >= 2 && signal.podQOSClass == 'BestEffort' " +
				"&& signal.podPriorityClass == 'batch' && signal.podUID != ''",
		},
	}

	crashLooping := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", UID: "uid-1"},
		Spec: corev1.PodSpec{
			PriorityClassName: "batch",
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSBestEffort,
			ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 1},
				{RestartCount: 1},
			},
		},
	}
	if !r.celConditionPasses(policy, crashLooping) {
		t.Error("CEL extended vars should pass for crash-looping best-effort pod")
	}

	healthy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid-2"},
		Status:     corev1.PodStatus{QOSClass: corev1.PodQOSGuaranteed},
	}
	if r.celConditionPasses(policy, healthy) {
		t.Error("CEL extended vars should fail for healthy guaranteed pod")
	}
}

func TestSetPolicyErrorHandler(t *testing.T) {
	r := NewResolver(nil, nil, "node-1")

	var gotPolicy string
	var gotErr error
	r.SetPolicyErrorHandler(func(name string, err error) {
		gotPolicy = name
		gotErr = err
	})

	policy := &v1alpha1.SchedulingPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec: v1alpha1.SchedulingPolicySpec{
			CELCondition: "this is not CEL",
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}

	if r.celConditionPasses(policy, pod) {
		t.Error("invalid CEL must not pass")
	}
	if gotPolicy != "broken" {
		t.Errorf("error handler policy = %q, want %q", gotPolicy, "broken")
	}
	if gotErr == nil {
		t.Error("error handler should receive the CEL error")
	}
}
