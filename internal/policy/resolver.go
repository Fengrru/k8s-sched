// Package policy resolves SchedulingPolicy CRDs into per-pod
// scheduling parameters. It watches SchedulingPolicy CRDs, evaluates
// label selectors and CEL conditions, and merges policy-defined params
// with pod annotation overrides.
package policy

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/fengrru/k8s-sched/api/v1alpha1"
	"github.com/fengrru/k8s-sched/internal/cel"
	"github.com/fengrru/k8s-sched/internal/maps"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Params holds the resolved scheduling parameters for a single pod.
// Zero values mean "not set by any policy".
type Params struct {
	Weight   uint64
	BudgetNs uint64
}

// Resolver watches SchedulingPolicy CRDs and resolves scheduling
// parameters for pods by matching label selectors and evaluating
// optional CEL conditions.
type Resolver struct {
	log       *zap.Logger
	clientset kubernetes.Interface
	nodeName  string
	celCache  *cel.Cache

	// onPolicyError is invoked when a policy fails to compile or
	// evaluate its CEL condition, so callers can surface Kubernetes
	// Events without the resolver depending on a cluster client.
	onPolicyError func(policyName string, err error)

	mu          sync.RWMutex
	policies    map[string]*v1alpha1.SchedulingPolicy
	matchedPods map[string]map[string]int32 // policy name -> set of pod UIDs
}

// NewResolver creates a policy resolver.
func NewResolver(
	log *zap.Logger,
	clientset kubernetes.Interface,
	nodeName string,
) *Resolver {
	if log == nil {
		log = zap.NewNop()
	}
	return &Resolver{
		log:         log,
		clientset:   clientset,
		nodeName:    nodeName,
		celCache:    cel.NewCache(256),
		policies:    make(map[string]*v1alpha1.SchedulingPolicy),
		matchedPods: make(map[string]map[string]int32),
	}
}

// SchedulingPolicyGVR is the GroupVersionResource for the SchedulingPolicy CRD.
var SchedulingPolicyGVR = schema.GroupVersionResource{
	Group:    "scheduling.fengrru.dev",
	Version:  "v1alpha1",
	Resource: "schedulingpolicies",
}

// Start begins watching SchedulingPolicy CRDs via a dynamic informer.
// This provides real-time, event-driven policy updates with no polling.
func (r *Resolver) Start(stopCh <-chan struct{}) {
	dynClient, err := r.newDynamicClient()
	if err != nil {
		r.log.Warn("cannot create dynamic client, falling back to polling",
			zap.Error(err))
		go r.pollPolicies(stopCh)
		return
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynClient, 5*time.Minute, metav1.NamespaceAll, nil,
	)

	informer := factory.ForResource(SchedulingPolicyGVR).Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			policy := r.unstructuredToPolicy(obj)
			if policy == nil {
				return
			}
			r.addPolicy(policy)
		},
		UpdateFunc: func(_, newObj interface{}) {
			policy := r.unstructuredToPolicy(newObj)
			if policy == nil {
				return
			}
			r.addPolicy(policy)
		},
		DeleteFunc: func(obj interface{}) {
			policy := r.unstructuredToPolicy(obj)
			if policy == nil {
				return
			}
			r.removePolicy(policy.Name)
		},
	}); err != nil {
		r.log.Warn("cannot register policy event handler, falling back to polling",
			zap.Error(err))
		go r.pollPolicies(stopCh)
		return
	}

	go informer.Run(stopCh)

	// Wait for initial sync so policies are available before pod processing.
	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		r.log.Warn("policy informer cache sync failed, falling back to polling")
		go r.pollPolicies(stopCh)
		return
	}

	r.log.Info("policy resolver started (dynamic informer)",
		zap.String("node", r.nodeName),
	)
}

// newDynamicClient creates a dynamic client from in-cluster config.
func (r *Resolver) newDynamicClient() (dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return dynamic.NewForConfig(config)
}

// unstructuredToPolicy converts an unstructured object to a typed SchedulingPolicy.
func (r *Resolver) unstructuredToPolicy(obj interface{}) *v1alpha1.SchedulingPolicy {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		// Handle tombstone for delete events.
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			u, ok = tombstone.Obj.(*unstructured.Unstructured)
			if !ok {
				return nil
			}
		} else {
			return nil
		}
	}

	policy := &v1alpha1.SchedulingPolicy{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), policy)
	if err != nil {
		r.log.Warn("failed to convert unstructured to SchedulingPolicy",
			zap.String("name", u.GetName()),
			zap.Error(err))
		return nil
	}
	return policy
}

// addPolicy inserts or updates a policy in the cache.
func (r *Resolver) addPolicy(policy *v1alpha1.SchedulingPolicy) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.policies[policy.Name]
	if ok && existing.ResourceVersion == policy.ResourceVersion {
		return // no change
	}
	r.policies[policy.Name] = policy
	r.log.Debug("policy updated",
		zap.String("policy", policy.Name),
		zap.Int32("weight", policy.Spec.Weight),
	)
}

// removePolicy removes a policy from the cache.
func (r *Resolver) removePolicy(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.policies, name)
	delete(r.matchedPods, name)
	r.log.Debug("policy removed", zap.String("policy", name))
}

// pollPolicies is a fallback polling mechanism used when the dynamic
// informer cannot be created (e.g., outside cluster).
func (r *Resolver) pollPolicies(stopCh <-chan struct{}) {
	r.log.Warn("using polling-based policy refresh (10s interval)")
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Do an immediate initial refresh.
	r.refreshPolicies()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.refreshPolicies()
		}
	}
}

// refreshPolicies fetches all SchedulingPolicy CRDs via REST and updates cache.
// This is the fallback path used when the dynamic informer is unavailable.
func (r *Resolver) refreshPolicies() {
	req := r.clientset.CoreV1().RESTClient().Get().
		AbsPath("/apis/scheduling.fengrru.dev/v1alpha1/schedulingpolicies").
		SetHeader("Accept", "application/json")

	result := &v1alpha1.SchedulingPolicyList{}
	err := req.Do(context.Background()).Into(result)
	if err != nil {
		r.log.Debug("failed to list SchedulingPolicies",
			zap.Error(err),
		)
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	seen := make(map[string]bool)
	for i := range result.Items {
		policy := &result.Items[i]
		key := policy.Name
		seen[key] = true

		if existing, ok := r.policies[key]; ok {
			if existing.ResourceVersion == policy.ResourceVersion {
				continue
			}
		}
		r.policies[key] = policy
		r.log.Debug("policy refreshed",
			zap.String("policy", key),
			zap.Int32("weight", policy.Spec.Weight),
		)
	}

	for key := range r.policies {
		if !seen[key] {
			delete(r.policies, key)
			r.log.Debug("policy removed", zap.String("policy", key))
		}
	}
}

// SetPolicyErrorHandler registers a callback invoked whenever a policy's
// CEL condition fails to compile or evaluate.
func (r *Resolver) SetPolicyErrorHandler(h func(policyName string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onPolicyError = h
}

// qosDefaultWeight maps a pod's QoS class to the weight applied when
// no matching policy and no annotation sets one. This gives
// best-effort work a low priority and guaranteed work the default
// priority with zero configuration — the flat vtime pool always
// favors latency-critical pods.
func qosDefaultWeight(qos corev1.PodQOSClass) uint64 {
	switch qos {
	case corev1.PodQOSGuaranteed:
		return 1000
	case corev1.PodQOSBurstable:
		return 800
	case corev1.PodQOSBestEffort:
		return 200
	default:
		return 1000
	}
}

// Resolve returns the combined scheduling parameters for a pod.
// It merges all matching SchedulingPolicy CRDs with pod annotation
// overrides. Annotations always take precedence over CRD params.
//
// Resolution order:
//  1. All matching SchedulingPolicy CRDs: max weight wins, max budget wins.
//     Policies may set weights below the default (e.g. 200) to deprioritize;
//     the default only applies when no matching policy sets a weight.
//  2. Pod annotation (weight / budget-microseconds / importance)
//  3. QoS-based default: Guaranteed=1000, Burstable=800, BestEffort=200
func (r *Resolver) Resolve(pod *corev1.Pod) Params {
	result := Params{Weight: qosDefaultWeight(pod.Status.QOSClass)}

	policies := r.getMatchingPolicies(pod)
	matchedNames := make([]string, 0, len(policies))
	var policyWeight uint64
	for _, policy := range policies {
		matchedNames = append(matchedNames, policy.Name)
		if policy.Spec.Weight > 0 {
			if uint64(policy.Spec.Weight) > policyWeight {
				policyWeight = uint64(policy.Spec.Weight)
			}
		}
		if policy.Spec.BudgetMicroseconds > 0 {
			budgetNs := uint64(policy.Spec.BudgetMicroseconds) * 1000
			if budgetNs > result.BudgetNs {
				result.BudgetNs = budgetNs
			}
		}
	}
	// A policy-set weight replaces the default outright, so policies can
	// lower priority below 1000, not only raise it.
	if policyWeight > 0 {
		result.Weight = policyWeight
	}

	if pod.Annotations != nil {
		if w := maps.ParseAnnotationWeight(pod.Annotations); w > 0 {
			result.Weight = w
		}
		if b := maps.ParseAnnotationBudget(pod.Annotations); b > 0 {
			result.BudgetNs = b
		}
	}

	r.trackMatchedPods(matchedNames, string(pod.UID))

	return result
}

// trackMatchedPods records which policies match a given pod UID.
func (r *Resolver) trackMatchedPods(matchedPolicyNames []string, podUID string) {
	if podUID == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove this pod from all policies it no longer matches.
	for name, pids := range r.matchedPods {
		if _, matched := r.policies[name]; !matched {
			delete(r.matchedPods, name)
			continue
		}
		delete(pids, podUID)
		if len(pids) == 0 {
			delete(r.matchedPods, name)
		}
	}

	// Add to currently matching policies.
	inSet := make(map[string]bool, len(matchedPolicyNames))
	for _, name := range matchedPolicyNames {
		inSet[name] = true
		if r.matchedPods[name] == nil {
			r.matchedPods[name] = make(map[string]int32)
		}
		r.matchedPods[name][podUID] = 1
	}
}

// getMatchingPolicies returns all policies whose podSelector matches
// the pod's labels AND whose CEL condition (if any) evaluates to true.
func (r *Resolver) getMatchingPolicies(pod *corev1.Pod) []*v1alpha1.SchedulingPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var matched []*v1alpha1.SchedulingPolicy
	for _, policy := range r.policies {
		if !r.podSelectorMatches(policy, pod) {
			continue
		}
		if !r.celConditionPasses(policy, pod) {
			continue
		}
		matched = append(matched, policy)
	}
	return matched
}

// podSelectorMatches checks if the policy's podSelector matches the pod's labels.
func (r *Resolver) podSelectorMatches(policy *v1alpha1.SchedulingPolicy, pod *corev1.Pod) bool {
	if policy.Spec.PodSelector == nil {
		// Empty selector matches all pods.
		return true
	}

	sel, err := metav1.LabelSelectorAsSelector(policy.Spec.PodSelector)
	if err != nil {
		r.log.Warn("invalid podSelector in policy",
			zap.String("policy", policy.Name),
			zap.Error(err),
		)
		return false
	}

	return sel.Matches(labels.Set(pod.Labels))
}

// celConditionPasses evaluates the policy's optional CEL condition.
func (r *Resolver) celConditionPasses(policy *v1alpha1.SchedulingPolicy, pod *corev1.Pod) bool {
	if policy.Spec.CELCondition == "" {
		return true
	}

	cpuRequest, cpuLimit := aggregatePodCPU(pod)

	vars := map[string]interface{}{
		"signal": map[string]interface{}{
			"podName":          pod.Name,
			"podNamespace":     pod.Namespace,
			"podUID":           string(pod.UID),
			"podQOSClass":      string(pod.Status.QOSClass),
			"podPriorityClass": pod.Spec.PriorityClassName,
			"podRestartCount":  totalRestarts(pod),
			"podCPURequest":    cpuRequest,
			"podCPULimit":      cpuLimit,
			"nodeName":         pod.Spec.NodeName,
		},
		"context": map[string]interface{}{
			"policyName": policy.Name,
			// Exposed as doubles so expressions can mix them with
			// fractional literals (CEL has no int*double overload).
			"weight":   float64(policy.Spec.Weight),
			"budgetUs": float64(policy.Spec.BudgetMicroseconds),
		},
	}

	ok, err := r.celCache.Evaluate(policy.Spec.CELCondition, vars)
	if err != nil {
		r.log.Warn("CEL evaluation failed",
			zap.String("policy", policy.Name),
			zap.String("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)),
			zap.String("expr", policy.Spec.CELCondition),
			zap.Error(err),
		)
		if r.onPolicyError != nil {
			r.onPolicyError(policy.Name, err)
		}
		return false
	}

	return ok
}

// totalRestarts sums RestartCount across all container statuses.
func totalRestarts(pod *corev1.Pod) int64 {
	var n int64
	for i := range pod.Status.ContainerStatuses {
		n += int64(pod.Status.ContainerStatuses[i].RestartCount)
	}
	return n
}

// aggregatePodCPU sums CPU requests and limits across all containers.
func aggregatePodCPU(pod *corev1.Pod) (request, limit float64) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if q := c.Resources.Requests.Cpu(); q != nil {
			request += float64(q.MilliValue()) / 1000.0
		}
		if q := c.Resources.Limits.Cpu(); q != nil {
			limit += float64(q.MilliValue()) / 1000.0
		}
	}
	if math.IsNaN(request) || math.IsNaN(limit) {
		return 0, 0
	}
	return request, limit
}

// ActivePodCount returns the number of pods currently matching a policy.
func (r *Resolver) ActivePodCount(policyName string) int32 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if pids, ok := r.matchedPods[policyName]; ok {
		return int32(len(pids))
	}
	return 0
}

// PolicyNames returns the names of all currently cached policies.
func (r *Resolver) PolicyNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.policies))
	for name := range r.policies {
		names = append(names, name)
	}
	return names
}

// PolicyGeneration returns the generation of a cached policy, or false
// if it is not present.
func (r *Resolver) PolicyGeneration(name string) (int64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.policies[name]
	if !ok {
		return 0, false
	}
	return p.Generation, true
}
