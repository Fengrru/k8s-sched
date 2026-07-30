package maps

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExtractSchedulingParams_Empty(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
	sp := extractSchedulingParams(pod)

	if sp.Weight != defaultWeight {
		t.Errorf("empty annotations: want weight=%d, got %d", defaultWeight, sp.Weight)
	}
	if sp.BudgetNs != defaultBudgetNs {
		t.Errorf("empty annotations: want budget=0, got %d", sp.BudgetNs)
	}
}

func TestExtractSchedulingParams_NilAnnotations(t *testing.T) {
	pod := &corev1.Pod{}
	sp := extractSchedulingParams(pod)

	if sp.Weight != defaultWeight {
		t.Errorf("nil annotations: want weight=%d, got %d", defaultWeight, sp.Weight)
	}
}

func TestExtractSchedulingParams_Importance(t *testing.T) {
	tests := []struct {
		name       string
		importance string
		wantWeight uint64
	}{
		{"max", "100", 10000},
		{"default-mid", "50", 5000},
		{"min", "1", 100},
		{"out-of-range-clamped", "101", defaultWeight},
		{"zero-ignored", "0", defaultWeight},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						annotationImportance: tt.importance,
					},
				},
			}
			sp := extractSchedulingParams(pod)
			if sp.Weight != tt.wantWeight {
				t.Errorf("importance=%s: want weight=%d, got %d",
					tt.importance, tt.wantWeight, sp.Weight)
			}
		})
	}
}

func TestExtractSchedulingParams_Weight(t *testing.T) {
	tests := []struct {
		name       string
		weight     string
		wantWeight uint64
	}{
		{"explicit", "9000", 9000},
		{"minimum", "1", 1},
		{"maximum", "10000", 10000},
		{"negative", "-5", defaultWeight},
		{"out-of-range", "10001", defaultWeight},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						annotationWeight: tt.weight,
					},
				},
			}
			sp := extractSchedulingParams(pod)
			if sp.Weight != tt.wantWeight {
				t.Errorf("weight=%s: want weight=%d, got %d",
					tt.weight, tt.wantWeight, sp.Weight)
			}
		})
	}
}

func TestExtractSchedulingParams_WeightOverridesImportance(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				annotationImportance: "95", // 95*100 = 9500
				annotationWeight:     "2000",
			},
		},
	}
	sp := extractSchedulingParams(pod)

	if sp.Weight != 2000 {
		t.Errorf("weight should override importance: want 2000, got %d", sp.Weight)
	}
}

func TestExtractSchedulingParams_Budget(t *testing.T) {
	tests := []struct {
		name   string
		budget string
		wantNs uint64
	}{
		{"micro-to-nano", "20000", 20000000},
		{"zero", "0", 0},
		{"invalid", "abc", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						annotationBudget: tt.budget,
					},
				},
			}
			sp := extractSchedulingParams(pod)
			if sp.BudgetNs != tt.wantNs {
				t.Errorf("budget=%s: want %d ns, got %d ns",
					tt.budget, tt.wantNs, sp.BudgetNs)
			}
		})
	}
}

func TestExtractSchedulingParams_Combined(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				annotationWeight: "8000",
				annotationBudget: "50000",
			},
		},
	}
	sp := extractSchedulingParams(pod)

	if sp.Weight != 8000 {
		t.Errorf("combined: want weight=8000, got %d", sp.Weight)
	}
	if sp.BudgetNs != 50000000 {
		t.Errorf("combined: want budget=50000000ns, got %d", sp.BudgetNs)
	}
}

func TestResolvePodPIDs_NilPod(t *testing.T) {
	pids := resolvePodPIDs(nil)
	if pids != nil {
		t.Error("resolvePodPIDs(nil) should return nil")
	}
}

func TestResolvePodPIDs_EmptyUID(t *testing.T) {
	pod := &corev1.Pod{}
	pids := resolvePodPIDs(pod)
	if pids != nil {
		t.Error("resolvePodPIDs with empty UID should return nil")
	}
}

func TestParseCgroupProcs(t *testing.T) {
	tests := []struct {
		name string
		data string
		want []int32
	}{
		{"single", "12345", []int32{12345}},
		{"multiple", "100\n200\n300", []int32{100, 200, 300}},
		{"trailing newline", "1\n2\n", []int32{1, 2}},
		{"empty lines", "1\n\n2\n", []int32{1, 2}},
		{"empty", "", nil},
		{"invalid pid", "abc", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCgroupProcs([]byte(tt.data))
			if len(got) != len(tt.want) {
				t.Errorf("parseCgroupProcs() len = %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseCgroupProcs()[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseAnnotationWeight_Exported(t *testing.T) {
	// Verify exported ParseAnnotationWeight returns 0 when not set (no default).
	if w := ParseAnnotationWeight(nil); w != 0 {
		t.Errorf("ParseAnnotationWeight(nil) = %d, want 0", w)
	}

	ann := map[string]string{"scheduling.fengrru.dev/weight": "5000"}
	if w := ParseAnnotationWeight(ann); w != 5000 {
		t.Errorf("ParseAnnotationWeight(weight=5000) = %d, want 5000", w)
	}
}

func TestParseAnnotationBudget_Exported(t *testing.T) {
	// Verify exported ParseAnnotationBudget returns 0 when not set.
	if b := ParseAnnotationBudget(nil); b != 0 {
		t.Errorf("ParseAnnotationBudget(nil) = %d, want 0", b)
	}

	ann := map[string]string{"scheduling.fengrru.dev/budget-microseconds": "10000"}
	if b := ParseAnnotationBudget(ann); b != 10000000 {
		t.Errorf("ParseAnnotationBudget(budget=10000) = %d, want 10000000", b)
	}
}

func TestExtractSchedulingParams_UsesParsedFunctions(t *testing.T) {
	// Verify extractSchedulingParams correctly uses ParseAnnotationWeight/Budget
	// and applies defaults when nothing is set.
	pod := &corev1.Pod{}
	sp := extractSchedulingParams(pod)
	if sp.Weight != 1000 {
		t.Errorf("default weight = %d, want 1000", sp.Weight)
	}
	if sp.BudgetNs != 0 {
		t.Errorf("default budget = %d, want 0", sp.BudgetNs)
	}
}
