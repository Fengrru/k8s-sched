package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sp

// SchedulingPolicy translates Pod labels into kernel-level CPU
// scheduling parameters via sched_ext.
//
// The DaemonSet agent reads these policies, resolves matching Pods to
// cgroup IDs (falling back to PIDs), and writes {weight, budget_ns} to
// the BPF cgroup_params/task_params maps. The in-kernel sched_ext
// scheduler then uses these values for weighted virtual-time dispatch
// and per-slice budget capping.
//
// Example:
//
//	apiVersion: scheduling.fengrru.dev/v1alpha1
//	kind: SchedulingPolicy
//	metadata:
//	  name: latency-critical
//	spec:
//	  podSelector:
//	    matchLabels:
//	      app: payment-api
//	  weight: 9000
//	  budgetMicroseconds: 2000
type SchedulingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SchedulingPolicySpec   `json:"spec,omitempty"`
	Status SchedulingPolicyStatus `json:"status,omitempty"`
}

// SchedulingPolicySpec defines the desired scheduling parameters.
//
// Two orthogonal dimensions:
//   - weight: relative CPU allocation (1-10000). Influences vtime.
//     Higher weight = task runs more frequently relative to peers.
//     Weights below the 1000 default deprioritize a workload.
//   - budgetMicroseconds: absolute cap per scheduling slice.
//     0 = no cap. Non-zero = maximum contiguous CPU time before preemption
//     (slice = min(default 5ms, budget); values >= 5000 have no effect).
//
// Optional celCondition enables dynamic policy evaluation:
//   - Variables: signal (pod CPU request/limit, name, namespace, node),
//     context (policy weight, budget, name). weight and budgetUs are
//     exposed as doubles so expressions can mix them with fractional
//     literals (CEL has no int*double overload).
//   - Example: "signal.podCPURequest > context.budgetUs * 0.001"
type SchedulingPolicySpec struct {
	// podSelector matches Pods to which this policy applies.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// weight is the scheduling weight (1-10000).
	// 10000 = highest priority, 1 = lowest, 1000 = default (applied
	// only when no matching policy sets a weight).
	// Controls how frequently the task runs relative to others.
	// +kubebuilder:validation:Description="Scheduling weight. Higher = more CPU time; values below 1000 deprioritize a workload. 1000 is the default applied when no matching policy sets a weight."
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +optional
	Weight int32 `json:"weight,omitempty"`

	// budgetMicroseconds caps the maximum CPU time per scheduling slice:
	// slice = min(default 5ms slice, budget). 0 = unlimited (the implicit
	// default when the field is omitted; do not set a CRD default, the
	// resolver treats 0 as unset).
	// Example: 2000 = task runs at most 2ms before being preempted.
	// Values >= 5000 (the default slice) have no effect.
	// +kubebuilder:validation:Description="Max CPU time per scheduling slice (caps the 5ms default slice; values >= 5000 have no effect). 0 = unlimited."
	// +optional
	BudgetMicroseconds int64 `json:"budgetMicroseconds,omitempty"`

	// celCondition is an optional CEL expression that must evaluate to true
	// for the policy to apply. Evaluated per-pod with the following variables:
	//   - signal: pod-level state and resource info
	//       signal.podName          string   - pod name
	//       signal.podNamespace     string   - pod namespace
	//       signal.podUID           string   - pod UID
	//       signal.podQOSClass      string   - QoS class (Guaranteed/Burstable/BestEffort)
	//       signal.podPriorityClass string   - priorityClassName, empty when unset
	//       signal.podRestartCount  int      - total container restarts
	//       signal.podCPURequest    double   - total CPU request across containers (cores)
	//       signal.podCPULimit      double   - total CPU limit across containers (cores)
	//       signal.nodeName         string   - node the pod is scheduled on
	//   - context: policy-level metadata
	//       context.policyName   string  - name of this SchedulingPolicy
	//       context.weight       double  - policy weight (0 when unset)
	//       context.budgetUs     double  - policy budget in microseconds (0 = no cap)
	// Example: "signal.podCPURequest > context.budgetUs * 0.001"
	// +kubebuilder:validation:Description="Optional CEL expression that must evaluate to true for the policy to apply. Variables: signal.podName/podNamespace/podUID (string), signal.podQOSClass (Guaranteed/Burstable/BestEffort), signal.podPriorityClass (string), signal.podRestartCount (int), signal.podCPURequest/podCPULimit (double, cores), signal.nodeName (string), context.policyName (string), context.weight/context.budgetUs (double, 0 when unset)."
	// +optional
	CELCondition string `json:"celCondition,omitempty"`
}

// SchedulingPolicyStatus reports the observed state of the policy.
type SchedulingPolicyStatus struct {
	// activePods is the number of Pods currently matching this policy
	// on this node.
	// +kubebuilder:validation:Description="Number of pods matching this policy."
	// +optional
	ActivePods int32 `json:"activePods,omitempty"`

	// nodeStatuses reports per-node matching pod counts, keyed first by
	// node name then by policy name. Each node's agent writes its own
	// entry, so `kubectl get sp` shows where the policy is actually
	// taking effect.
	// +kubebuilder:validation:Description="Per-node matching pod counts (node -> policy -> count). Written by each node's agent."
	// +optional
	NodeStatuses map[string]map[string]int32 `json:"nodeStatuses,omitempty"`

	// conditions represent the latest available observations of the policy's state.
	// +kubebuilder:validation:Description="Current state conditions of the policy."
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the most recent generation observed for this policy.
	// +kubebuilder:validation:Description="Most recent generation observed for this policy."
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true

// SchedulingPolicyList contains a list of SchedulingPolicy.
type SchedulingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SchedulingPolicy `json:"items"`
}
