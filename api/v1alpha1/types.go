package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sp

// SchedulingPolicy translates Pod labels into kernel-level CPU
// scheduling parameters via sched_ext.
//
// The DaemonSet agent reads these policies, resolves matching Pods
// to host PIDs, and writes {weight, budget_ns} to the BPF task_params
// map. The in-kernel sched_ext scheduler then uses these values for
// weighted virtual-time dispatch and per-slice budget capping.
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
//   - budgetMicroseconds: absolute cap per scheduling slice.
//     0 = no cap. Non-zero = maximum contiguous CPU time before preemption
//     (slice = min(default 5ms, budget); values >= 5000 have no effect).
//
// Optional celCondition enables dynamic policy evaluation:
//   - Variables: signal (pod CPU request/limit, name, namespace, node),
//     context (policy weight, budget, name)
//   - Example: "signal.podCPURequest > context.budgetUs * 0.001"
type SchedulingPolicySpec struct {
	// podSelector matches Pods to which this policy applies.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// weight is the scheduling weight (1-10000).
	// 10000 = highest priority, 1 = lowest, 1000 = default.
	// Controls how frequently the task runs relative to others.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	// +kubebuilder:default=1000
	// +optional
	Weight int32 `json:"weight,omitempty"`

	// budgetMicroseconds caps the maximum CPU time per scheduling slice:
	// slice = min(default 5ms slice, budget). 0 = unlimited.
	// Example: 2000 = task runs at most 2ms before being preempted.
	// Values >= 5000 (the default slice) have no effect.
	// +kubebuilder:default=0
	// +optional
	BudgetMicroseconds int64 `json:"budgetMicroseconds,omitempty"`

	// celCondition is an optional CEL expression that must evaluate to true
	// for the policy to apply. Evaluated per-pod with the following variables:
	//   - signal: pod-level state and resource info
	//       signal.podName       string   - pod name
	//       signal.podNamespace  string   - pod namespace
	//       signal.podCPURequest float64  - total CPU request across containers (cores)
	//       signal.podCPULimit   float64  - total CPU limit across containers (cores)
	//       signal.nodeName      string   - node the pod is scheduled on
	//   - context: policy-level metadata
	//       context.policyName   string   - name of this SchedulingPolicy
	//       context.weight       int32    - policy weight
	//       context.budgetUs     int64    - policy budget in microseconds
	// Example: "signal.podCPURequest > 0.5"
	// +optional
	CELCondition string `json:"celCondition,omitempty"`
}

// SchedulingPolicyStatus reports the observed state of the policy.
type SchedulingPolicyStatus struct {
	// activePods is the number of Pods currently matching this policy
	// on this node.
	// +optional
	ActivePods int32 `json:"activePods,omitempty"`

	// conditions represent the latest available observations of the policy's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the most recent generation observed for this policy.
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
