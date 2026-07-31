package k8s

import (
	"fmt"
	"reflect"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// NewInClusterClient creates a K8s client using in-cluster config.
func NewInClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

// PodCallback is invoked when a pod is added/updated or deleted.
type PodCallback func(*corev1.Pod)

// PodWatcher watches Pods on a specific node and invokes callbacks
// when pod lifecycle events occur. It also caches SchedulingPolicy
// CRDs to derive scheduling parameters.
type PodWatcher struct {
	log       *zap.Logger
	clientset kubernetes.Interface
	nodeName  string
	onAdd     PodCallback
	onDelete  PodCallback
}

// NewPodWatcher creates a pod watcher for a specific node.
func NewPodWatcher(
	log *zap.Logger,
	clientset kubernetes.Interface,
	nodeName string,
	onAdd PodCallback,
	onDelete PodCallback,
) *PodWatcher {
	return &PodWatcher{
		log:       log,
		clientset: clientset,
		nodeName:  nodeName,
		onAdd:     onAdd,
		onDelete:  onDelete,
	}
}

// Start begins watching pods on the specified node.
func (w *PodWatcher) Start(stopCh <-chan struct{}) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset,
		0,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("spec.nodeName=%s", w.nodeName)
		}),
	)

	informer := factory.Core().V1().Pods().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			w.handlePodUpdate(pod)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				return
			}
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				return
			}
			// Relists and status-only updates (e.g. resourceVersion
			// bumps from annotations) would otherwise re-run Resolve
			// and churn the BPF maps. Only react when something the
			// scheduler depends on actually changed.
			if !podRelevantFieldsChanged(oldPod, pod) {
				return
			}
			w.handlePodUpdate(pod)
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			w.onDelete(pod)
		},
	}); err != nil {
		w.log.Error("failed to register pod event handler", zap.Error(err))
		return
	}

	go informer.Run(stopCh)

	w.log.Info("pod watcher started",
		zap.String("node", w.nodeName),
	)
}

func (w *PodWatcher) handlePodUpdate(pod *corev1.Pod) {
	switch pod.Status.Phase {
	case corev1.PodRunning:
		// Running (or transitioned to running): (re)apply params.
		if w.onAdd != nil {
			w.onAdd(pod)
		}
	case corev1.PodSucceeded, corev1.PodFailed:
		// Terminal: containers are gone, release the BPF entries.
		if w.onDelete != nil {
			w.onDelete(pod)
		}
	}
}

// podRelevantFieldsChanged reports whether an update touches anything
// the scheduler consumes: phase, labels, annotations, or container IDs.
func podRelevantFieldsChanged(oldPod, newPod *corev1.Pod) bool {
	if oldPod.Status.Phase != newPod.Status.Phase {
		return true
	}
	if !reflect.DeepEqual(oldPod.Labels, newPod.Labels) {
		return true
	}
	if !reflect.DeepEqual(oldPod.Annotations, newPod.Annotations) {
		return true
	}
	return !reflect.DeepEqual(containerIDs(oldPod), containerIDs(newPod))
}

// containerIDs returns the container IDs of a pod in status order,
// used to detect container (re)starts without re-resolving on every
// status heartbeat.
func containerIDs(pod *corev1.Pod) []string {
	var ids []string
	for _, cs := range pod.Status.ContainerStatuses {
		ids = append(ids, cs.ContainerID)
	}
	return ids
}
