// Package metrics exposes Prometheus metrics for the k8s-sched agent.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ActivePolicies is the number of active SchedulingPolicy CRDs.
	ActivePolicies = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sched_active_policies",
		Help: "Number of active SchedulingPolicy CRDs",
	})

	// EnqueueCount is the total number of task enqueues observed by
	// the in-kernel scheduler (mirrors the BPF stats counter).
	EnqueueCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_sched_enqueues_total",
		Help: "Total number of task enqueues into the scheduler's vtime queue",
	})

	// BudgetCapped counts enqueues where a CPU budget shortened the
	// slice, i.e. the task will be preempted earlier than default.
	BudgetCapped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_sched_budget_capped_total",
		Help: "Total number of enqueues with the slice capped by a CPU budget",
	})

	// LocalDispatches counts wakeups that found an idle CPU and were
	// inserted directly into its local DSQ, bypassing the vtime queue.
	LocalDispatches = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_sched_local_dispatches_total",
		Help: "Total number of idle-CPU direct dispatches bypassing the vtime queue",
	})

	// SchedulerLoaded reports whether the sched_ext scheduler is
	// attached (1) or the agent is running in observe-only mode (0).
	SchedulerLoaded = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sched_scheduler_loaded",
		Help: "1 when the sched_ext scheduler is attached, 0 in observe-only mode",
	})

	// ParamsMapped is the number of pods with scheduling params applied.
	ParamsMapped = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sched_params_mapped",
		Help: "Number of pods with active scheduling parameter mappings",
	})

	// AttachRetries counts attach attempts that failed because a
	// previous k8s-sched instance still owns the scheduler (rolling
	// upgrade handover).
	AttachRetries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_sched_attach_retries_total",
		Help: "Total number of scheduler attach retries during rolling upgrade handover",
	})

	// StatusWritebacks counts successful SchedulingPolicy status
	// write-backs (per-node matching pod counts).
	StatusWritebacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_sched_status_writebacks_total",
		Help: "Total number of SchedulingPolicy status write-backs",
	})
)
