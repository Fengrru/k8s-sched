// Package maps manages BPF map access from Go userspace.
//
// Maps are shared between the BPF scheduler (in-kernel) and the
// Go agent (userspace). The agent writes scheduling parameters;
// the BPF scheduler reads them at enqueue/tick time.
package maps

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	corev1 "k8s.io/api/core/v1"
)

// ProcRoot is the filesystem path for /proc.
// When running in a container with hostPID, the host /proc is
// typically mounted at /host/proc. Set via HOST_PROC env var.
var ProcRoot = initProcRoot()

func initProcRoot() string {
	if p := os.Getenv("HOST_PROC"); p != "" {
		return p
	}
	// Containerized deployments mount the host /proc at /host/proc.
	// This runs at package init, before main() — an os.Setenv in main()
	// would be too late to influence it, so probe the path directly.
	if _, err := os.Stat("/host/proc"); err == nil {
		return "/host/proc"
	}
	return "/proc"
}

type TaskParams struct {
	Weight   uint64
	BudgetNs uint64
}

// SchedStats mirrors struct sched_stats in k8s_sched.bpf.c.
type SchedStats struct {
	Enqueues     uint64
	BudgetCapped uint64
	Defaults     uint64
}

type Maps struct {
	TaskParams *ebpf.Map
	Stats      *ebpf.Map

	mu      sync.Mutex
	podPIDs map[string][]uint32 // pod UID -> PIDs written to the map
}

// Pin paths where the BPF maps are pinned by the loader.
const (
	pinPath      = "/sys/fs/bpf/k8s-sched/task_params"
	statsPinPath = "/sys/fs/bpf/k8s-sched/stats"
)

// New returns an empty Maps. Call Open() after the scheduler has
// loaded and pinned the BPF maps to connect.
func New() *Maps {
	return &Maps{podPIDs: make(map[string][]uint32)}
}

// Open connects to pinned BPF maps. Safe to call multiple times.
func (m *Maps) Open() error {
	tp, err := ebpf.LoadPinnedMap(pinPath, nil)
	if err != nil {
		return fmt.Errorf("open %s: %w (scheduler not loaded yet?)", pinPath, err)
	}
	m.TaskParams = tp

	// Stats map is optional: metrics export degrades gracefully without it.
	if st, err := ebpf.LoadPinnedMap(statsPinPath, nil); err == nil {
		m.Stats = st
	}
	return nil
}

// ReadStats reads the in-kernel scheduler counters.
func (m *Maps) ReadStats() (SchedStats, error) {
	var s SchedStats
	if m.Stats == nil {
		return s, fmt.Errorf("stats map not open")
	}
	var key uint32
	if err := m.Stats.Lookup(&key, &s); err != nil {
		return s, fmt.Errorf("lookup stats: %w", err)
	}
	return s, nil
}

// TrackedPods returns the number of pods with PIDs recorded in the map.
func (m *Maps) TrackedPods() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.podPIDs)
}

func (m *Maps) UpdatePodParams(pod *corev1.Pod, resolved ...SchedParams) error {
	if m.TaskParams == nil {
		return nil
	}
	var params SchedParams
	if len(resolved) > 0 {
		params = resolved[0]
	} else {
		params = extractSchedulingParams(pod)
	}
	pids := resolvePodPIDs(pod)

	var firstErr error
	written := make([]uint32, 0, len(pids))
	for _, pid := range pids {
		pidKey := uint32(pid)
		tp := TaskParams{Weight: params.Weight, BudgetNs: params.BudgetNs}
		if err := m.TaskParams.Put(&pidKey, &tp); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("put pid %d: %w", pid, err)
			}
			continue
		}
		written = append(written, pidKey)
	}

	// Remember which PIDs we wrote: by deletion time the pod's cgroup
	// is usually gone and PID re-resolution would return nothing.
	if pod != nil && pod.UID != "" {
		m.mu.Lock()
		m.podPIDs[string(pod.UID)] = written
		m.mu.Unlock()
	}
	return firstErr
}

func (m *Maps) RemovePodParams(pod *corev1.Pod) {
	if m.TaskParams == nil || pod == nil {
		return
	}

	uid := string(pod.UID)
	m.mu.Lock()
	recorded := m.podPIDs[uid]
	delete(m.podPIDs, uid)
	m.mu.Unlock()

	seen := make(map[uint32]bool, len(recorded))
	for _, pid := range recorded {
		seen[pid] = true
	}
	for _, pid := range resolvePodPIDs(pod) {
		seen[uint32(pid)] = true
	}
	for pid := range seen {
		pidKey := pid
		m.TaskParams.Delete(&pidKey) //nolint:errcheck // best-effort; stale PIDs are swept periodically
	}
}

// SchedParams holds the scheduling parameters for a pod.
type SchedParams struct {
	Weight   uint64
	BudgetNs uint64
}

// ---- Scheduling parameter extraction ----

type schedParams struct {
	weight   uint64
	budgetNs uint64
}

const (
	defaultWeight   uint64 = 1000
	defaultBudgetNs        = 0
)

const (
	annotationWeight     = "scheduling.fengrru.dev/weight"
	annotationBudget     = "scheduling.fengrru.dev/budget-microseconds"
	annotationImportance = "scheduling.fengrru.dev/importance"
)

// ExtractSchedulingParams extracts scheduling parameters from a pod's annotations.
// Returns default weight (1000) when no annotations are set.
func ExtractSchedulingParams(pod *corev1.Pod) SchedParams {
	return extractSchedulingParams(pod)
}

// ParseAnnotationWeight extracts weight from annotations without applying defaults.
// Returns 0 if no weight or importance annotation is set.
// This is useful when merging with CRD-based policies where you need to
// distinguish "not set" from "set to default".
func ParseAnnotationWeight(ann map[string]string) uint64 {
	if ann == nil {
		return 0
	}
	// Explicit weight overrides importance.
	if w := ann[annotationWeight]; w != "" {
		if v, err := strconv.ParseUint(w, 10, 64); err == nil && v >= 1 && v <= 10000 {
			return v
		}
	}
	// Importance (1-100) converted to weight (importance × 100).
	if imp := ann[annotationImportance]; imp != "" {
		if v, err := strconv.ParseUint(imp, 10, 64); err == nil && v > 0 && v <= 100 {
			return v * 100
		}
	}
	return 0
}

// ParseAnnotationBudget extracts budget from annotations in nanoseconds.
// Returns 0 if no budget annotation is set.
func ParseAnnotationBudget(ann map[string]string) uint64 {
	if ann == nil {
		return 0
	}
	if b := ann[annotationBudget]; b != "" {
		if v, err := strconv.ParseUint(b, 10, 64); err == nil && v > 0 {
			return v * 1000 // microseconds → nanoseconds
		}
	}
	return 0
}

// ResolvePodPIDs finds all host PIDs belonging to a pod.
func ResolvePodPIDs(pod *corev1.Pod) []int32 {
	return resolvePodPIDs(pod)
}

func extractSchedulingParams(pod *corev1.Pod) SchedParams {
	ann := pod.Annotations
	sp := schedParams{weight: defaultWeight, budgetNs: defaultBudgetNs}

	if w := ParseAnnotationWeight(ann); w > 0 {
		sp.weight = w
	}
	if b := ParseAnnotationBudget(ann); b > 0 {
		sp.budgetNs = b
	}

	return SchedParams{Weight: sp.weight, BudgetNs: sp.budgetNs}
}

// ---- Pod PID resolution via cgroup v2 ----

// resolvePodPIDs finds all host PIDs belonging to a pod.
//
// Primary path: reads cgroup.procs from the pod's cgroup v2 directory.
// This is O(1) per pod instead of O(/proc size).
//
// Fallback: scans /proc/<pid>/cgroup for the pod UID marker.
// Used when cgroup v2 path is not available.
func resolvePodPIDs(pod *corev1.Pod) []int32 {
	if pod == nil {
		return nil
	}

	uid := string(pod.UID)
	if uid == "" {
		return nil
	}

	// Try cgroup v2 fast path first.
	if pids := resolveViaCgroupV2(uid); len(pids) > 0 {
		return pids
	}

	// Fall back to /proc scanning.
	return resolveViaProcScan(uid)
}

// cgroupV2Base is the root of the cgroup v2 filesystem.
// Override via CGROUP_V2_ROOT env var for non-standard mounts.
var cgroupV2Base = initCgroupV2Base()

func initCgroupV2Base() string {
	if p := os.Getenv("CGROUP_V2_ROOT"); p != "" {
		return p
	}
	return "/sys/fs/cgroup"
}

// resolveViaCgroupV2 attempts to find the pod's PIDs via cgroup v2
// cgroup.procs files. It tries common cgroup paths for Kubernetes pods.
func resolveViaCgroupV2(podUID string) []int32 {
	// Common QoS class paths under cgroup v2 with systemd driver.
	// Format: /sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/
	//         kubepods-<qos>-pod<UID>.slice/cgroup.procs
	suffixes := []string{
		"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + podUID + ".slice/cgroup.procs",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + podUID + ".slice/cgroup.procs",
		"kubepods.slice/kubepods-guaranteed.slice/kubepods-guaranteed-pod" + podUID + ".slice/cgroup.procs",
		// cgroupfs driver (no systemd slices).
		"kubepods/pod" + podUID + "/cgroup.procs",
	}

	for _, suffix := range suffixes {
		path := cgroupV2Base + "/" + suffix
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return parseCgroupProcs(data)
	}

	return nil
}

// parseCgroupProcs parses the content of a cgroup.procs file
// which contains one PID per line.
func parseCgroupProcs(data []byte) []int32 {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	pids := make([]int32, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.ParseInt(line, 10, 32)
		if err != nil {
			continue
		}
		pids = append(pids, int32(pid))
	}
	return pids
}

// resolveViaProcScan scans /proc/<pid>/cgroup for the pod UID marker.
// This is the legacy fallback path.
//
// Uses a short-lived TTL cache of the /proc directory listing to avoid
// repeated os.ReadDir calls during bulk pod updates. Cache TTL is 5s.
func resolveViaProcScan(podUID string) []int32 {
	pidMarker := fmt.Sprintf("pod%s", podUID)

	entries := getProcDirCache()

	var pids []int32
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil {
			continue
		}

		cgroupPath := ProcRoot + "/" + entry.Name() + "/cgroup"
		data, err := os.ReadFile(cgroupPath)
		if err != nil {
			continue
		}

		if strings.Contains(string(data), pidMarker) {
			pids = append(pids, int32(pid))
		}
	}

	return pids
}

// ---- /proc directory listing cache ----

var (
	procCacheMu     sync.RWMutex
	procCache       []os.DirEntry
	procCacheExpiry time.Time
	procCacheTTL    = 5 * time.Second
)

func getProcDirCache() []os.DirEntry {
	procCacheMu.RLock()
	if time.Now().Before(procCacheExpiry) && procCache != nil {
		entries := procCache
		procCacheMu.RUnlock()
		return entries
	}
	procCacheMu.RUnlock()

	procCacheMu.Lock()
	defer procCacheMu.Unlock()

	// Double-check after acquiring write lock.
	if time.Now().Before(procCacheExpiry) && procCache != nil {
		return procCache
	}

	entries, err := os.ReadDir(ProcRoot)
	if err != nil {
		return nil
	}
	procCache = entries
	procCacheExpiry = time.Now().Add(procCacheTTL)
	return procCache
}
