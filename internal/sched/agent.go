// Package sched manages the sched_ext scheduler lifecycle and
// Kubernetes pod → scheduling parameter mapping.
//
// Architecture:
//
//	PodWatcher ──→ PolicyResolver ──→ BPF Maps
//	                  │
//	          SchedulingPolicy CRDs
//	          CEL expression cache
//	          Annotation overrides
package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	"github.com/fengrru/k8s-sched/api/v1alpha1"
	"github.com/fengrru/k8s-sched/internal/k8s"
	"github.com/fengrru/k8s-sched/internal/maps"
	"github.com/fengrru/k8s-sched/internal/metrics"
	"github.com/fengrru/k8s-sched/internal/policy"
)

const (
	mapPinDir           = "/sys/fs/bpf/k8s-sched"
	defaultMetricsPort  = ":9090"
	pidCleanupFreq      = 30 * time.Second
	statsExportFreq     = 15 * time.Second
	statusWritebackFreq = 15 * time.Second
)

// attachRetryInterval is the pause between scheduler attach attempts
// during a rolling-upgrade handover. A var (not const) so tests can
// shorten it.
var attachRetryInterval = 2 * time.Second

// HandoffDelay is how long the agent keeps the sched_ext scheduler
// attached after SIGTERM. During a rolling upgrade the replacement
// agent can then attach while the old scheduler is still running,
// so the node never falls back to EEVDF. Must stay below the pod
// terminationGracePeriodSeconds (default 30s).
const HandoffDelay = 15 * time.Second

// Agent is the per-node control loop: it watches pods and policies,
// resolves scheduling parameters, writes them into the BPF maps, and
// owns the sched_ext scheduler link and the metrics/health server.
type Agent struct {
	log         *zap.Logger
	nodeName    string
	clientset   kubernetes.Interface
	mapOps      *maps.Maps
	watcher     *k8s.PodWatcher
	resolver    *policy.Resolver
	schedLink   link.Link
	metricsAddr string

	// loadSchedFn is the attach function, overridable in tests to
	// exercise the rolling-upgrade handover retry loop.
	loadSchedFn func() error

	// opsPath is the sched_ext registered-ops file, overridable in
	// tests (empty = default path).
	opsPath string

	// Cluster-facing reporting (best-effort, nil outside a cluster).
	dynClient   dynamic.Interface
	recorder    record.EventRecorder
	broadcaster record.EventBroadcaster

	// Metrics & health
	ready      atomic.Bool
	metricsSrv *http.Server
}

// NewAgent constructs an Agent wired to the cluster (in-cluster config
// when available) for the given node. It does not load the scheduler;
// call Run for that.
func NewAgent(log *zap.Logger, nodeName, metricsAddr string) (*Agent, error) {
	if metricsAddr == "" {
		metricsAddr = defaultMetricsPort
	}
	clientset, err := k8s.NewInClusterClient()
	if err != nil {
		return nil, fmt.Errorf("create k8s client: %w", err)
	}

	mapOps := maps.New()

	// PolicyResolver merges CRD-based policies with pod annotations.
	resolver := policy.NewResolver(log, clientset, nodeName)

	a := &Agent{
		log:         log,
		nodeName:    nodeName,
		clientset:   clientset,
		mapOps:      mapOps,
		resolver:    resolver,
		metricsAddr: metricsAddr,
	}
	a.loadSchedFn = a.loadScheduler

	// Events: surface policy failures as Kubernetes Events so
	// operators see why a policy is not applying.
	if broadcaster, rec := newEventRecorder(clientset); broadcaster != nil {
		a.broadcaster = broadcaster
		a.recorder = rec
	}
	resolver.SetPolicyErrorHandler(func(policyName string, err error) {
		a.emitEvent(policyEventObject(policyName), "PolicyCelError",
			"CEL condition failed to compile or evaluate: "+err.Error())
	})

	// Dynamic client for SchedulingPolicy status write-back. Best
	// effort: tests and out-of-cluster runs simply skip write-back.
	if config, err := rest.InClusterConfig(); err == nil {
		if dyn, err := dynamic.NewForConfig(config); err == nil {
			a.dynClient = dyn
		}
	}

	// Pod callback: resolve policies → write to BPF map.
	onPodUpdate := func(pod *corev1.Pod) {
		// Resolve scheduling params from CRDs + annotations.
		resolved := resolver.Resolve(pod)
		sp := maps.SchedParams{
			Weight:   resolved.Weight,
			BudgetNs: resolved.BudgetNs,
		}
		if err := mapOps.UpdatePodParams(pod, sp); err != nil {
			log.Warn("update pod params",
				zap.String("pod", pod.Namespace+"/"+pod.Name),
				zap.Error(err))
			a.emitEvent(pod, "PodResolveError",
				"failed to resolve scheduling parameters: "+err.Error())
		}

		// Update metrics.
		metrics.ActivePolicies.Set(float64(len(resolver.PolicyNames())))
		metrics.ParamsMapped.Set(float64(mapOps.TrackedPods()))
	}

	onPodDelete := func(pod *corev1.Pod) {
		mapOps.RemovePodParams(pod)
		metrics.ParamsMapped.Set(float64(mapOps.TrackedPods()))
	}

	watcher := k8s.NewPodWatcher(
		log, clientset, nodeName,
		onPodUpdate,
		onPodDelete,
	)
	a.watcher = watcher

	return a, nil
}

// newEventRecorder wires an EventBroadcaster to the cluster and
// returns the broadcaster plus a recorder. Returns nil, nil when the
// cluster client is unavailable (e.g. unit tests).
func newEventRecorder(clientset kubernetes.Interface) (record.EventBroadcaster, record.EventRecorder) {
	if clientset == nil {
		return nil, nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	recorder := broadcaster.NewRecorder(
		scheme.Scheme,
		corev1.EventSource{Component: "k8s-sched"},
	)
	return broadcaster, recorder
}

// emitEvent records a Kubernetes Event on the involved object
// (best-effort; silently skipped without a recorder).
func (a *Agent) emitEvent(obj runtime.Object, reason, message string) {
	if a.recorder == nil {
		return
	}
	a.recorder.Event(obj, corev1.EventTypeWarning, reason, message)
}

// policyEventObject builds a minimal SchedulingPolicy object carrying
// TypeMeta so the Event recorder can serialize it (the type is not
// registered in the client-go scheme).
func policyEventObject(name string) runtime.Object {
	return &v1alpha1.SchedulingPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "SchedulingPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// Run loads the scheduler (falling back to observe-only mode if it
// cannot attach), starts the watchers and metrics server, and blocks
// until ctx is canceled, then tears everything down.
func (a *Agent) Run(ctx context.Context) error {
	schedLoaded := false
	if err := a.loadSchedulerWithRetry(ctx); err != nil {
		a.log.Warn("scheduler not loaded, running in observe-only mode",
			zap.Error(err))
		a.emitEvent(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: a.nodeName}},
			"SchedulerLoadFailed",
			"sched_ext scheduler failed to attach, running in observe-only mode: "+err.Error())
	} else {
		schedLoaded = true
		defer func() { _ = a.schedLink.Close() }()
		defer func() { _ = os.RemoveAll(mapPinDir) }()
		if err := a.mapOps.Open(); err != nil {
			a.log.Warn("cannot open BPF maps", zap.Error(err))
		}
	}
	// Degraded mode stays visible to operators: alert on this gauge.
	if schedLoaded {
		metrics.SchedulerLoaded.Set(1)
	} else {
		metrics.SchedulerLoaded.Set(0)
	}

	// Start policy resolver (watches SchedulingPolicy CRDs).
	stopCh := make(chan struct{})
	if a.resolver != nil {
		a.resolver.Start(stopCh)
	}

	// Start pod watcher.
	if a.watcher != nil {
		a.watcher.Start(stopCh)
	}

	// Start metrics + health HTTP server.
	a.startMetricsServer()

	// Start periodic stale PID cleanup, BPF stats export, and
	// SchedulingPolicy status write-back.
	go a.periodicPIDCleanup(ctx)
	go a.periodicStatsExport(ctx)
	go a.periodicPolicyStatusWriteback(ctx)

	// Mark readiness from the scheduler state: in observe-only mode
	// (no BPF scheduler attached) the agent is alive but degraded, so
	// /readyz returns 503 and the pod drops out of Service endpoints
	// (the Service sets publishNotReadyAddresses to keep metrics
	// scrapeable in that state).
	a.ready.Store(schedLoaded)

	a.log.Info("agent running",
		zap.String("node", a.nodeName),
		zap.Bool("scheduler_loaded", schedLoaded),
		zap.String("metrics_addr", a.metricsAddr),
	)

	<-ctx.Done()

	a.log.Info("agent shutting down")
	close(stopCh)

	if a.broadcaster != nil {
		a.broadcaster.Shutdown()
	}

	if a.metricsSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.metricsSrv.Shutdown(shutdownCtx)
	}

	return nil
}

// loadSchedulerWithRetry attempts to attach the scheduler. If another
// k8s-sched instance still owns the struct_ops (rolling upgrade), the
// old agent keeps the scheduler alive while the new one retries — the
// node never falls back to EEVDF. Retries only while the contended
// scheduler is ours; foreign schedulers are respected (observe-only).
func (a *Agent) loadSchedulerWithRetry(ctx context.Context) error {
	if a.loadSchedFn == nil {
		a.loadSchedFn = a.loadScheduler
	}
	for {
		if err := a.loadSchedFn(); err == nil {
			return nil
		} else if !a.schedulerContended() || ctx.Err() != nil {
			return err
		}
		metrics.AttachRetries.Inc()
		a.log.Info("scheduler attach contended by previous instance, retrying",
			zap.Duration("interval", attachRetryInterval))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(attachRetryInterval):
		}
	}
}

// schedulerContended reports whether a k8s-sched scheduler is
// currently registered with sched_ext — i.e. the attach failure is a
// rolling-upgrade handover, not a foreign scheduler.
func (a *Agent) schedulerContended() bool {
	path := a.opsPath
	if path == "" {
		path = "/sys/kernel/sched_ext/root/ops"
	}
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), "k8s_sched")
}

// loadScheduler parses the BPF .o, loads maps+programs, pins maps
// under /sys/fs/bpf/k8s-sched/, and attaches the sched_ext struct_ops.
func (a *Agent) loadScheduler() error {
	objPath := os.Getenv("SCHED_BPF_OBJ")
	if objPath == "" {
		objPath = "/etc/k8s-sched/k8s_sched.bpf.o"
	}

	if _, err := os.Stat(objPath); err != nil {
		return fmt.Errorf("bpf object not found at %s: %w", objPath, err)
	}

	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return fmt.Errorf("parse BPF spec: %w", err)
	}

	if mkErr := os.MkdirAll(mapPinDir, 0o700); mkErr != nil {
		return fmt.Errorf("create pin dir %s: %w", mapPinDir, mkErr)
	}

	// Mark maps for pinning so they survive the collection lifecycle.
	// Only pin the maps that userspace needs to access.
	pinMaps := map[string]bool{"task_params": true, "cgroup_params": true, "stats": true}
	for _, ms := range spec.Maps {
		if pinMaps[ms.Name] {
			ms.Pinning = ebpf.PinByName
		}
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: mapPinDir,
		},
	})
	if err != nil {
		return fmt.Errorf("load BPF collection: %w", err)
	}

	// Find the struct_ops link map.
	// SCX_OPS_DEFINE(k8s_sched, ...) creates a map named "k8s_sched"
	// in section .struct_ops.link.
	structOpsMap := coll.Maps["k8s_sched"]
	if structOpsMap == nil {
		coll.Close()
		return fmt.Errorf("struct_ops map 'k8s_sched' not found in %s", objPath)
	}

	// Attach the struct_ops to register the scheduler with the kernel.
	l, err := link.AttachStructOps(link.StructOpsOptions{
		Map: structOpsMap,
	})
	if err != nil {
		coll.Close()
		return fmt.Errorf("attach struct_ops: %w", err)
	}

	a.schedLink = l

	a.log.Info("sched_ext scheduler loaded",
		zap.String("node", a.nodeName),
		zap.String("obj", filepath.Base(objPath)),
		zap.String("maps", mapPinDir),
	)

	return nil
}

// startMetricsServer starts an HTTP server exposing Prometheus metrics
// and health check endpoints (/healthz, /readyz).
func (a *Agent) startMetricsServer() {
	mux := http.NewServeMux()

	addr := a.metricsAddr
	if addr == "" {
		addr = defaultMetricsPort
	}

	// Prometheus metrics endpoint.
	mux.Handle("/metrics", promhttp.Handler())

	// Health checks.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if a.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		}
	})

	// Debug endpoints: dump the live parameter maps and the agent's
	// bookkeeping, so `kubectl exec` replaces bpftool for inspection.
	mux.HandleFunc("/debug/params", func(w http.ResponseWriter, r *http.Request) {
		if a.mapOps == nil {
			http.Error(w, "maps not open", http.StatusServiceUnavailable)
			return
		}
		dump, err := a.mapOps.DumpParams()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dump)
	})
	mux.HandleFunc("/debug/pods", func(w http.ResponseWriter, r *http.Request) {
		if a.mapOps == nil {
			http.Error(w, "maps not open", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a.mapOps.PodDetails())
	})

	a.metricsSrv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		a.log.Info("metrics server starting", zap.String("addr", addr))
		if err := a.metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			a.log.Error("metrics server failed", zap.Error(err))
		}
	}()
}

// periodicPIDCleanup periodically removes stale entries from the BPF
// maps: PIDs whose processes are gone and cgroup IDs whose cgroup
// directories no longer exist.
func (a *Agent) periodicPIDCleanup(ctx context.Context) {
	ticker := time.NewTicker(pidCleanupFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanStalePIDs()
			if a.mapOps != nil {
				if removed := a.mapOps.CleanStaleCgroupIDs(); removed > 0 {
					a.log.Info("cleaned stale cgroup IDs", zap.Int("count", removed))
				}
			}
		}
	}
}

// periodicPolicyStatusWriteback periodically reports this node's
// per-policy matching pod counts into SchedulingPolicy.status.
func (a *Agent) periodicPolicyStatusWriteback(ctx context.Context) {
	if a.dynClient == nil || a.resolver == nil {
		return
	}

	ticker := time.NewTicker(statusWritebackFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.writebackPolicyStatus(ctx)
		}
	}
}

// writebackPolicyStatus merges this node's policy -> matching pod
// count map into SchedulingPolicy.status.nodeStatuses. A merge patch
// updates only this node's entry, preserving other nodes' entries;
// the per-node policy map is replaced wholesale, which also drops
// counts for policies deleted locally.
func (a *Agent) writebackPolicyStatus(ctx context.Context) {
	names := a.resolver.PolicyNames()
	if len(names) == 0 {
		return
	}

	policyCounts := make(map[string]int32, len(names))
	for _, name := range names {
		policyCounts[name] = a.resolver.ActivePodCount(name)
	}

	patch, err := buildStatusPatch(a.nodeName, policyCounts)
	if err != nil {
		a.log.Warn("marshal status write-back patch", zap.Error(err))
		return
	}

	// Patch each policy's status. Policies without an entry are
	// skipped: they were never written (or were deleted, in which
	// case the node entry was already replaced by the next write).
	for _, name := range names {
		_, err := a.dynClient.Resource(policy.SchedulingPolicyGVR).
			Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, "status")
		if err != nil {
			a.log.Debug("status write-back failed",
				zap.String("policy", name),
				zap.Error(err))
		}
	}
	metrics.StatusWritebacks.Add(float64(len(names)))
}

// buildStatusPatch returns the merge-patch body that upserts this
// node's policy -> matching pod count map under
// status.nodeStatuses. The per-node policy map is replaced wholesale
// (dropping locally-deleted policies) while other nodes' entries are
// preserved by the merge semantics.
func buildStatusPatch(nodeName string, policyCounts map[string]int32) ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"status": map[string]interface{}{
			"nodeStatuses": map[string]interface{}{
				nodeName: policyCounts,
			},
		},
	})
}

// periodicStatsExport mirrors the in-kernel stats counters
// (enqueues, budget-capped slices) into Prometheus metrics.
func (a *Agent) periodicStatsExport(ctx context.Context) {
	if a.mapOps == nil {
		return
	}

	ticker := time.NewTicker(statsExportFreq)
	defer ticker.Stop()

	var last maps.SchedStats
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := a.mapOps.ReadStats()
			if err != nil {
				continue
			}
			if s.Enqueues >= last.Enqueues {
				metrics.EnqueueCount.Add(float64(s.Enqueues - last.Enqueues))
			}
			if s.BudgetCapped >= last.BudgetCapped {
				metrics.BudgetCapped.Add(float64(s.BudgetCapped - last.BudgetCapped))
			}
			if s.LocalDispatches >= last.LocalDispatches {
				metrics.LocalDispatches.Add(float64(s.LocalDispatches - last.LocalDispatches))
			}
			last = s
		}
	}
}

// cleanStalePIDs iterates the BPF task_params map and removes
// entries for PIDs that no longer exist on the host.
func (a *Agent) cleanStalePIDs() {
	if a.mapOps == nil || a.mapOps.TaskParams == nil {
		return
	}

	var key uint32
	var val maps.TaskParams
	iter := a.mapOps.TaskParams.Iterate()

	var stalePIDs []uint32
	for iter.Next(&key, &val) {
		pidPath := fmt.Sprintf("%s/%d", maps.ProcRoot, key)
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			stalePIDs = append(stalePIDs, key)
		}
	}

	for _, pid := range stalePIDs {
		if err := a.mapOps.TaskParams.Delete(&pid); err == nil {
			a.log.Debug("cleaned stale PID", zap.Uint32("pid", pid))
		}
	}

	if len(stalePIDs) > 0 {
		a.log.Info("cleaned stale PIDs", zap.Int("count", len(stalePIDs)))
	}
}
