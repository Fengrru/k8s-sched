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

	// ParamsMapped is the number of pods with scheduling params applied.
	ParamsMapped = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "k8s_sched_params_mapped",
		Help: "Number of pods with active scheduling parameter mappings",
	})
)
