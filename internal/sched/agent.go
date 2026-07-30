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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/fengrru/k8s-sched/internal/k8s"
	"github.com/fengrru/k8s-sched/internal/maps"
	"github.com/fengrru/k8s-sched/internal/metrics"
	"github.com/fengrru/k8s-sched/internal/policy"
)

const (
	mapPinDir          = "/sys/fs/bpf/k8s-sched"
	defaultMetricsPort = ":9090"
	pidCleanupFreq     = 30 * time.Second
	statsExportFreq    = 15 * time.Second
)

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

	// Metrics & health
	ready      atomic.Bool
	metricsSrv *http.Server
}

// NewAgent constructs an Agent wired to the cluster (in-cluster config
// when available) for the given node. It does not load the scheduler;
// call Run for that.
func NewAgent(ctx context.Context, log *zap.Logger, nodeName, metricsAddr string) (*Agent, error) {
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

	return &Agent{
		log:         log,
		nodeName:    nodeName,
		clientset:   clientset,
		mapOps:      mapOps,
		watcher:     watcher,
		resolver:    resolver,
		metricsAddr: metricsAddr,
	}, nil
}

// Run loads the scheduler (falling back to observe-only mode if it
// cannot attach), starts the watchers and metrics server, and blocks
// until ctx is canceled, then tears everything down.
func (a *Agent) Run(ctx context.Context) error {
	schedLoaded := false
	if err := a.loadScheduler(); err != nil {
		a.log.Warn("scheduler not loaded, running in observe-only mode",
			zap.Error(err))
	} else {
		schedLoaded = true
		defer func() { _ = a.schedLink.Close() }()
		defer func() { _ = os.RemoveAll(mapPinDir) }()
		if err := a.mapOps.Open(); err != nil {
			a.log.Warn("cannot open BPF maps", zap.Error(err))
		}
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

	// Start periodic stale PID cleanup and BPF stats export.
	go a.periodicPIDCleanup(ctx)
	go a.periodicStatsExport(ctx)

	// Mark as ready.
	a.ready.Store(true)

	a.log.Info("agent running",
		zap.String("node", a.nodeName),
		zap.Bool("scheduler_loaded", schedLoaded),
		zap.String("metrics_addr", a.metricsAddr),
	)

	<-ctx.Done()

	a.log.Info("agent shutting down")
	close(stopCh)

	if a.metricsSrv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.metricsSrv.Shutdown(shutdownCtx)
	}

	return nil
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

	// Load all maps, pin them so the maps package can open them later.
	// The .struct_ops map is special: it links the BPF ops to the kernel.
	opts := ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			PinPath: mapPinDir,
		},
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, opts)
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

// periodicPIDCleanup periodically removes stale PID entries from the
// BPF map for processes that no longer exist.
func (a *Agent) periodicPIDCleanup(ctx context.Context) {
	ticker := time.NewTicker(pidCleanupFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanStalePIDs()
		}
	}
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
