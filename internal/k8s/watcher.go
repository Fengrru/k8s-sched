package k8s

import (
	"fmt"

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
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			w.handlePodUpdate(pod)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
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
	})

	go informer.Run(stopCh)

	w.log.Info("pod watcher started",
		zap.String("node", w.nodeName),
	)
}

func (w *PodWatcher) handlePodUpdate(pod *corev1.Pod) {
	if pod.Status.Phase != corev1.PodRunning {
		return
	}
	w.onAdd(pod)
}
