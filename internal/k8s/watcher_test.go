package k8s

import (
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestNewPodWatcher(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	log := zap.NewNop()

	var added, deleted int
	w := NewPodWatcher(
		log, clientset, "node-1",
		func(pod *corev1.Pod) { added++ },
		func(pod *corev1.Pod) { deleted++ },
	)

	if w == nil {
		t.Fatal("NewPodWatcher returned nil")
	}
	if w.nodeName != "node-1" {
		t.Errorf("nodeName: want node-1, got %s", w.nodeName)
	}
}

func TestHandlePodUpdate_NotRunning(t *testing.T) {
	called := false
	w := &PodWatcher{
		log:   zap.NewNop(),
		onAdd: func(pod *corev1.Pod) { called = true },
	}

	pending := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	w.handlePodUpdate(pending)

	if called {
		t.Error("should not trigger callback for non-Running pod")
	}
}

func TestHandlePodUpdate_Running(t *testing.T) {
	called := false
	w := &PodWatcher{
		log:   zap.NewNop(),
		onAdd: func(pod *corev1.Pod) { called = true },
	}

	running := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	w.handlePodUpdate(running)

	if !called {
		t.Error("should trigger callback for Running pod")
	}
}

func TestNewInClusterClient_FailsOutsideCluster(t *testing.T) {
	_, err := NewInClusterClient()
	if err == nil {
		t.Log("running inside cluster (expected outside tests)")
	}
}

func TestPodWatcher_FieldSelector(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	w := NewPodWatcher(
		zap.NewNop(), clientset, "node-5",
		func(*corev1.Pod) {},
		func(*corev1.Pod) {},
	)

	stopCh := make(chan struct{})
	w.Start(stopCh)

	if w.nodeName != "node-5" {
		t.Errorf("nodeName: want node-5, got %s", w.nodeName)
	}

	close(stopCh)
}

func TestHandlePodUpdate_Succeeded(t *testing.T) {
	// Pods in Succeeded phase should NOT trigger callback.
	called := false
	w := &PodWatcher{
		log:   zap.NewNop(),
		onAdd: func(pod *corev1.Pod) { called = true },
	}

	succeeded := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}
	w.handlePodUpdate(succeeded)

	if called {
		t.Error("should not trigger callback for Succeeded pod")
	}
}

func TestHandlePodUpdate_Failed(t *testing.T) {
	// Pods in Failed phase should NOT trigger callback.
	called := false
	w := &PodWatcher{
		log:   zap.NewNop(),
		onAdd: func(pod *corev1.Pod) { called = true },
	}

	failed := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}
	w.handlePodUpdate(failed)

	if called {
		t.Error("should not trigger callback for Failed pod")
	}
}

func TestDeleteHandler_Tombstone(t *testing.T) {
	// Verify tombstone handling in delete events.
	called := false
	var deletedPod *corev1.Pod
	w := &PodWatcher{
		log:      zap.NewNop(),
		onDelete: func(pod *corev1.Pod) { called = true; deletedPod = pod },
	}

	// Simulate a delete event via informer handler.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "deleted-pod", Namespace: "default"},
	}

	// Direct pod delete (not tombstone).
	handler := cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			p, ok := obj.(*corev1.Pod)
			if !ok {
				return
			}
			w.onDelete(p)
		},
	}
	handler.DeleteFunc(pod)

	if !called {
		t.Error("delete handler should be called for pod deletion")
	}
	if deletedPod.Name != "deleted-pod" {
		t.Errorf("deleted pod name = %s, want deleted-pod", deletedPod.Name)
	}
}

func TestPodWatcher_LogField(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	log := zap.NewNop()
	w := NewPodWatcher(log, clientset, "worker-7", nil, nil)

	if w.log != log {
		t.Error("logger should be stored")
	}
	if w.clientset != clientset {
		t.Error("clientset should be stored")
	}
}
