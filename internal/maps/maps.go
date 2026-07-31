// Package maps manages BPF map access from Go userspace.
//
// Maps are shared between the BPF scheduler (in-kernel) and the
// Go agent (userspace). The agent writes scheduling parameters;
// the BPF scheduler reads them at enqueue/tick time.
package maps

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// TaskParams mirrors struct task_params in k8s_sched.bpf.c: the
// per-task scheduling parameters the BPF scheduler reads at enqueue.
type TaskParams struct {
	Weight   uint64
	BudgetNs uint64
}

// SchedStats mirrors struct sched_stats in k8s_sched.bpf.c.
type SchedStats struct {
	Enqueues        uint64
	LocalDispatches uint64
	BudgetCapped    uint64
	Defaults        uint64
}

// Maps provides userspace access to the BPF maps that the loader
// pinned under /sys/fs/bpf/k8s-sched, plus bookkeeping of the cgroup
// IDs and PIDs written per pod so they can be removed on pod deletion.
type Maps struct {
	TaskParams   *ebpf.Map
	CgroupParams *ebpf.Map
	Stats        *ebpf.Map

	mu           sync.Mutex
	podPIDs      map[string][]uint32 // pod UID -> fallback PIDs written
	podCgroupIDs map[string][]uint64 // pod UID -> cgroup IDs written
}

// Pin paths where the BPF maps are pinned by the loader.
const (
	pinPath       = "/sys/fs/bpf/k8s-sched/task_params"
	cgroupPinPath = "/sys/fs/bpf/k8s-sched/cgroup_params"
	statsPinPath  = "/sys/fs/bpf/k8s-sched/stats"
)

// New returns an empty Maps. Call Open() after the scheduler has
// loaded and pinned the BPF maps to connect.
func New() *Maps {
	return &Maps{
		podPIDs:      make(map[string][]uint32),
		podCgroupIDs: make(map[string][]uint64),
	}
}

// Open connects to pinned BPF maps. Safe to call multiple times.
func (m *Maps) Open() error {
	tp, err := ebpf.LoadPinnedMap(pinPath, nil)
	if err != nil {
		return fmt.Errorf("open %s: %w (scheduler not loaded yet?)", pinPath, err)
	}
	m.TaskParams = tp

	// Cgroup map is the primary parameter channel; degrade to
	// PID-only writes when running against an older BPF object.
	if cg, err := ebpf.LoadPinnedMap(cgroupPinPath, nil); err == nil {
		m.CgroupParams = cg
	}

	// Stats map is optional: metrics export degrades gracefully without it.
	if st, err := ebpf.LoadPinnedMap(statsPinPath, nil); err == nil {
		m.Stats = st
	}
	return nil
}

// PodDetails returns the current bookkeeping snapshot: which cgroup IDs
// and PIDs were written per tracked pod. Used by the /debug endpoints.
func (m *Maps) PodDetails() []PodDetail {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Merge bookkeeping into per-UID entries.
	merged := make(map[string]*PodDetail)
	for uid, ids := range m.podCgroupIDs {
		merged[uid] = &PodDetail{UID: uid, CgroupIDs: append([]uint64(nil), ids...)}
	}
	for uid, pids := range m.podPIDs {
		d, ok := merged[uid]
		if !ok {
			d = &PodDetail{UID: uid}
			merged[uid] = d
		}
		d.PIDs = append([]uint32(nil), pids...)
	}

	out := make([]PodDetail, 0, len(merged))
	for _, d := range merged {
		out = append(out, *d)
	}
	return out
}

// PodDetail describes one tracked pod's bookkeeping snapshot.
type PodDetail struct {
	UID       string   `json:"uid"`
	CgroupIDs []uint64 `json:"cgroupIds,omitempty"`
	PIDs      []uint32 `json:"pids,omitempty"`
}

// ParamsDump is a snapshot of both BPF parameter maps for debugging.
type ParamsDump struct {
	CgroupEntries map[uint64]TaskParams `json:"cgroupEntries,omitempty"`
	PIDEntries    map[uint32]TaskParams `json:"pidEntries,omitempty"`
}

// DumpParams iterates the parameter maps and returns their contents.
func (m *Maps) DumpParams() (ParamsDump, error) {
	dump := ParamsDump{
		CgroupEntries: make(map[uint64]TaskParams),
		PIDEntries:    make(map[uint32]TaskParams),
	}
	if m.CgroupParams != nil {
		it := m.CgroupParams.Iterate()
		var key uint64
		var val TaskParams
		for it.Next(&key, &val) {
			dump.CgroupEntries[key] = val
		}
		if err := it.Err(); err != nil {
			return dump, fmt.Errorf("iterate cgroup_params: %w", err)
		}
	}
	if m.TaskParams != nil {
		it := m.TaskParams.Iterate()
		var key uint32
		var val TaskParams
		for it.Next(&key, &val) {
			dump.PIDEntries[key] = val
		}
		if err := it.Err(); err != nil {
			return dump, fmt.Errorf("iterate task_params: %w", err)
		}
	}
	return dump, nil
}

func (m *Maps) ReadStats() (SchedStats, error) {
	var out SchedStats
	if m.Stats == nil {
		return out, fmt.Errorf("stats map not open")
	}
	var key uint32
	var perCPU []SchedStats
	if err := m.Stats.Lookup(&key, &perCPU); err != nil {
		// Older BPF objects use a plain array map.
		if err2 := m.Stats.Lookup(&key, &out); err2 == nil {
			return out, nil
		}
		return out, fmt.Errorf("lookup stats: %w", err)
	}
	for _, s := range perCPU {
		out.Enqueues += s.Enqueues
		out.LocalDispatches += s.LocalDispatches
		out.BudgetCapped += s.BudgetCapped
		out.Defaults += s.Defaults
	}
	return out, nil
}

// TrackedPods returns the number of pods with parameters recorded in
// either the cgroup or the PID map.
func (m *Maps) TrackedPods() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	uids := make(map[string]bool, len(m.podCgroupIDs)+len(m.podPIDs))
	for uid := range m.podCgroupIDs {
		uids[uid] = true
	}
	for uid := range m.podPIDs {
		uids[uid] = true
	}
	return len(uids)
}

// UpdatePodParams writes scheduling parameters for the pod into the
// BPF maps. The preferred channel is one cgroup_params entry per
// cgroup in the pod's subtree (covers processes forked later); when
// the pod's cgroup cannot be found, it falls back to per-PID entries
// in task_params. An optional resolved SchedParams overrides
// annotation-derived values. Returns the first write error.
func (m *Maps) UpdatePodParams(pod *corev1.Pod, resolved ...SchedParams) error {
	if m.TaskParams == nil || pod == nil {
		return nil
	}
	var params SchedParams
	if len(resolved) > 0 {
		params = resolved[0]
	} else {
		params = extractSchedulingParams(pod)
	}
	tp := TaskParams(params)
	uid := string(pod.UID)

	var firstErr error

	// Preferred: cgroup-level entries for the whole pod subtree.
	if m.CgroupParams != nil {
		cgids := resolvePodCgroupIDs(uid)
		if len(cgids) > 0 {
			written := make([]uint64, 0, len(cgids))
			for _, cgid := range cgids {
				key := cgid
				if err := m.CgroupParams.Put(&key, &tp); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("put cgroup %d: %w", cgid, err)
					}
					continue
				}
				written = append(written, key)
			}
			if uid != "" {
				m.mu.Lock()
				m.podCgroupIDs[uid] = written
				m.mu.Unlock()
			}
			return firstErr
		}
	}

	// Fallback: per-PID entries from /proc scanning.
	pids := resolvePodPIDs(pod)
	written := make([]uint32, 0, len(pids))
	for _, pid := range pids {
		pidKey := uint32(pid)
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
	if uid != "" {
		m.mu.Lock()
		m.podPIDs[uid] = written
		m.mu.Unlock()
	}
	return firstErr
}

// RemovePodParams deletes the cgroup_params and task_params entries
// previously recorded for the pod (and any still resolvable now).
func (m *Maps) RemovePodParams(pod *corev1.Pod) {
	if m.TaskParams == nil || pod == nil {
		return
	}

	uid := string(pod.UID)
	m.mu.Lock()
	recordedPIDs := m.podPIDs[uid]
	recordedCgIDs := m.podCgroupIDs[uid]
	delete(m.podPIDs, uid)
	delete(m.podCgroupIDs, uid)
	m.mu.Unlock()

	if m.CgroupParams != nil {
		cgSeen := make(map[uint64]bool, len(recordedCgIDs))
		for _, cgid := range recordedCgIDs {
			cgSeen[cgid] = true
		}
		for _, cgid := range resolvePodCgroupIDs(uid) {
			cgSeen[cgid] = true
		}
		for cgid := range cgSeen {
			key := cgid
			m.CgroupParams.Delete(&key) //nolint:errcheck // best-effort; stale IDs are swept periodically
		}
	}

	seen := make(map[uint32]bool, len(recordedPIDs))
	for _, pid := range recordedPIDs {
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

// CleanStaleCgroupIDs removes cgroup_params entries whose cgroup no
// longer exists on the host (pod gone, agent missed the delete event).
// Returns the number of entries removed.
func (m *Maps) CleanStaleCgroupIDs() int {
	if m.CgroupParams == nil {
		return 0
	}
	live := liveKubepodsCgroupIDs()
	if len(live) == 0 {
		// Cannot see the kubepods tree (transient mount issue?):
		// deleting everything now would be worse than keeping
		// stale entries for one more sweep.
		return 0
	}

	var key uint64
	var val TaskParams
	var stale []uint64
	iter := m.CgroupParams.Iterate()
	for iter.Next(&key, &val) {
		if !live[key] {
			stale = append(stale, key)
		}
	}
	removed := 0
	for _, cgid := range stale {
		k := cgid
		if err := m.CgroupParams.Delete(&k); err == nil {
			removed++
		}
	}
	return removed
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
	defaultBudgetNs uint64 = 0
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

// ---- Pod PID / cgroup resolution via cgroup v2 ----

// resolvePodPIDs finds all host PIDs belonging to a pod.
//
// Primary path: walks the pod's cgroup v2 subtree and reads every
// cgroup.procs. cgroup v2 forbids processes in non-leaf cgroups, so
// the pod-level file is empty and the PIDs live in the container
// scope leaves.
//
// Fallback: scans /proc/<pid>/cgroup for the pod UID marker.
// Used when the pod's cgroup directory cannot be located.
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

// systemdUID converts a pod UID to the form used in systemd slice
// names: kubelet's systemd cgroup driver replaces dashes with
// underscores (kubepods-burstable-pod<uid_with_underscores>.slice).
func systemdUID(uid string) string {
	return strings.ReplaceAll(uid, "-", "_")
}

// podCgroupDirCandidates returns the possible cgroup v2 directories
// for a pod across cgroup drivers and QoS classes. Note that with the
// systemd driver, guaranteed pods sit directly under kubepods.slice
// (there is no kubepods-guaranteed.slice).
func podCgroupDirCandidates(uid string) []string {
	sysd := systemdUID(uid)
	return []string{
		// systemd cgroup driver (kubeadm and most distros' default).
		cgroupV2Base + "/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + sysd + ".slice",
		cgroupV2Base + "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + sysd + ".slice",
		cgroupV2Base + "/kubepods.slice/kubepods-pod" + sysd + ".slice",
		// cgroupfs driver (no systemd slices, raw UID with dashes).
		cgroupV2Base + "/kubepods/besteffort/pod" + uid,
		cgroupV2Base + "/kubepods/burstable/pod" + uid,
		cgroupV2Base + "/kubepods/pod" + uid,
	}
}

// findPodCgroupDir locates the pod's cgroup v2 directory, or "" when
// none of the known layouts match.
func findPodCgroupDir(uid string) string {
	for _, dir := range podCgroupDirCandidates(uid) {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
	}
	return ""
}

// resolveViaCgroupV2 collects the pod's PIDs by walking its cgroup
// subtree and reading every cgroup.procs file.
func resolveViaCgroupV2(podUID string) []int32 {
	dir := findPodCgroupDir(podUID)
	if dir == "" {
		return nil
	}

	var pids []int32
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip unreadable entries
		}
		data, rerr := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if rerr != nil {
			return nil
		}
		pids = append(pids, parseCgroupProcs(data)...)
		return nil
	})
	return pids
}

// ResolvePodCgroupIDs returns the cgroup IDs (kernfs inode numbers)
// of the pod's cgroup directory and all of its descendants. These are
// the keys of the BPF cgroup_params map.
func ResolvePodCgroupIDs(pod *corev1.Pod) []uint64 {
	if pod == nil {
		return nil
	}
	return resolvePodCgroupIDs(string(pod.UID))
}

func resolvePodCgroupIDs(uid string) []uint64 {
	if uid == "" {
		return nil
	}
	dir := findPodCgroupDir(uid)
	if dir == "" {
		return nil
	}

	var ids []uint64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best-effort: skip unreadable entries
		}
		if id, ok := dirCgroupID(path); ok {
			ids = append(ids, id)
		}
		return nil
	})
	return ids
}

// liveKubepodsCgroupIDs collects the cgroup IDs of every directory
// currently under the kubepods roots, for stale-entry sweeping.
func liveKubepodsCgroupIDs() map[uint64]bool {
	roots := []string{
		cgroupV2Base + "/kubepods.slice", // systemd driver
		cgroupV2Base + "/kubepods",       // cgroupfs driver
	}
	live := make(map[uint64]bool)
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil //nolint:nilerr // best-effort: skip unreadable entries
			}
			if id, ok := dirCgroupID(path); ok {
				live[id] = true
			}
			return nil
		})
	}
	return live
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
// The marker is matched in both raw (cgroupfs driver: pod<uid>) and
// systemd-slice form (pod<uid_with_underscores>), since the cgroup
// path in /proc/<pid>/cgroup follows the driver's naming.
//
// Uses a short-lived TTL cache of the /proc directory listing to avoid
// repeated os.ReadDir calls during bulk pod updates. Cache TTL is 5s.
func resolveViaProcScan(podUID string) []int32 {
	markers := []string{"pod" + podUID}
	if sysd := systemdUID(podUID); sysd != podUID {
		markers = append(markers, "pod"+sysd)
	}

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

		content := string(data)
		for _, marker := range markers {
			if strings.Contains(content, marker) {
				pids = append(pids, int32(pid))
				break
			}
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
