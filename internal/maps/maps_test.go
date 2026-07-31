package maps

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

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

// ---- cgroup resolution ----------------

// withCgroupBase swaps the package cgroup root for the duration of a test.
func withCgroupBase(t *testing.T, base string) {
	t.Helper()
	old := cgroupV2Base
	cgroupV2Base = base
	t.Cleanup(func() { cgroupV2Base = old })
}

// withStubDirCgroupID replaces dirCgroupID with a deterministic hash
// stub so cgroup-ID walks can be tested on any platform.
func withStubDirCgroupID(t *testing.T) {
	t.Helper()
	old := dirCgroupID
	dirCgroupID = func(path string) (uint64, bool) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(path))
		return h.Sum64(), true
	}
	t.Cleanup(func() { dirCgroupID = old })
}

func TestSystemdUID(t *testing.T) {
	tests := []struct {
		uid  string
		want string
	}{
		{"abc-def-123", "abc_def_123"},
		{"nodashes", "nodashes"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := systemdUID(tt.uid); got != tt.want {
			t.Errorf("systemdUID(%q) = %q, want %q", tt.uid, got, tt.want)
		}
	}
}

func TestPodCgroupDirCandidates(t *testing.T) {
	base := t.TempDir()
	withCgroupBase(t, base)

	cands := podCgroupDirCandidates("abcd-1234")
	want := []string{
		base + "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podabcd_1234.slice",
		base + "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podabcd_1234.slice",
		base + "/kubepods.slice/kubepods-podabcd_1234.slice", // guaranteed: no QoS sub-slice
		base + "/kubepods/besteffort/podabcd-1234",
		base + "/kubepods/burstable/podabcd-1234",
		base + "/kubepods/podabcd-1234",
	}
	if !reflect.DeepEqual(cands, want) {
		t.Errorf("podCgroupDirCandidates() = %v, want %v", cands, want)
	}
}

// TestResolveViaCgroupV2_WalksLeaves verifies the cgroup v2 "no
// internal processes" rule: the pod-level cgroup.procs is empty and
// the PIDs live in the container scope leaves.
func TestResolveViaCgroupV2_WalksLeaves(t *testing.T) {
	base := t.TempDir()
	withCgroupBase(t, base)

	podDir := filepath.Join(base,
		"kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-podabc_123.slice")
	if err := os.MkdirAll(filepath.Join(podDir, "cri-containerd-a.scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(podDir, "cri-containerd-b.scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pod-level file exists but is empty (no internal processes).
	if err := os.WriteFile(filepath.Join(podDir, "cgroup.procs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podDir, "cri-containerd-a.scope", "cgroup.procs"), []byte("100\n200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podDir, "cri-containerd-b.scope", "cgroup.procs"), []byte("300"), 0o644); err != nil {
		t.Fatal(err)
	}

	pids := resolveViaCgroupV2("abc-123")
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	want := []int32{100, 200, 300}
	if !reflect.DeepEqual(pids, want) {
		t.Errorf("resolveViaCgroupV2() = %v, want %v", pids, want)
	}
}

// TestResolveViaCgroupV2_GuaranteedDirect verifies guaranteed-QoS pods
// under the systemd driver sit directly under kubepods.slice.
func TestResolveViaCgroupV2_GuaranteedDirect(t *testing.T) {
	base := t.TempDir()
	withCgroupBase(t, base)

	podDir := filepath.Join(base, "kubepods.slice", "kubepods-podabc_123.slice")
	if err := os.MkdirAll(filepath.Join(podDir, "cri-containerd-a.scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(podDir, "cri-containerd-a.scope", "cgroup.procs"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}

	pids := resolveViaCgroupV2("abc-123")
	if len(pids) != 1 || pids[0] != 42 {
		t.Errorf("resolveViaCgroupV2(guaranteed) = %v, want [42]", pids)
	}
}

func TestResolvePodCgroupIDs_WalksSubtree(t *testing.T) {
	base := t.TempDir()
	withCgroupBase(t, base)
	withStubDirCgroupID(t)

	podDir := filepath.Join(base, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-podabc_123.slice")
	for _, leaf := range []string{"cri-containerd-a.scope", "cri-containerd-b.scope"} {
		if err := os.MkdirAll(filepath.Join(podDir, leaf), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ids := resolvePodCgroupIDs("abc-123")
	// Pod dir + two container leaves.
	if len(ids) != 3 {
		t.Errorf("resolvePodCgroupIDs() = %d ids, want 3 (pod + 2 leaves): %v", len(ids), ids)
	}
}

func TestResolvePodCgroupIDs_EmptyUID(t *testing.T) {
	if ids := resolvePodCgroupIDs(""); ids != nil {
		t.Errorf("resolvePodCgroupIDs(empty) = %v, want nil", ids)
	}
}

func TestLiveKubepodsCgroupIDs(t *testing.T) {
	base := t.TempDir()
	withCgroupBase(t, base)
	withStubDirCgroupID(t)

	// A live pod under the systemd driver and one under cgroupfs.
	dirs := []string{
		filepath.Join(base, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-podlive_1.slice"),
		filepath.Join(base, "kubepods", "burstable", "podlive-2"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	live := liveKubepodsCgroupIDs()
	// Every directory in the tree gets an ID (roots included).
	if len(live) < 6 {
		t.Errorf("liveKubepodsCgroupIDs() = %d ids, want >= 6", len(live))
	}
}

// TestResolveViaProcScan_SystemdAndRawMarkers verifies the fallback
// /proc scan matches both the raw UID (cgroupfs driver) and the
// underscore form (systemd driver) of the pod marker.
func TestResolveViaProcScan_SystemdAndRawMarkers(t *testing.T) {
	procRoot := t.TempDir()
	oldProc := ProcRoot
	ProcRoot = procRoot
	t.Cleanup(func() { ProcRoot = oldProc })

	// Invalidate the /proc listing cache.
	procCacheMu.Lock()
	procCache = nil
	procCacheExpiry = time.Time{}
	procCacheMu.Unlock()

	files := map[string]string{
		"100": "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podabc_123.slice/cri-containerd-x.scope\n",
		"200": "0::/kubepods/burstable/podabc-123/cri-containerd-y.scope\n",
		"300": "0::/other.slice/some.service\n",
	}
	for pid, cg := range files {
		if err := os.MkdirAll(filepath.Join(procRoot, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procRoot, pid, "cgroup"), []byte(cg), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pids := resolveViaProcScan("abc-123")
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	want := []int32{100, 200}
	if !reflect.DeepEqual(pids, want) {
		t.Errorf("resolveViaProcScan() = %v, want %v", pids, want)
	}
}
