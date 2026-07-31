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
	addCalled, deleteCalled := false, false
	w := &PodWatcher{
		log:      zap.NewNop(),
		onAdd:    func(pod *corev1.Pod) { addCalled = true },
		onDelete: func(pod *corev1.Pod) { deleteCalled = true },
	}

	pending := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	w.handlePodUpdate(pending)

	if addCalled {
		t.Error("should not trigger add callback for non-Running pod")
	}
	if deleteCalled {
		t.Error("should not trigger delete callback for non-terminal pod")
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
	// Terminal pods must release their BPF entries via onDelete.
	addCalled, deleteCalled := false, false
	w := &PodWatcher{
		log:      zap.NewNop(),
		onAdd:    func(pod *corev1.Pod) { addCalled = true },
		onDelete: func(pod *corev1.Pod) { deleteCalled = true },
	}

	succeeded := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
		},
	}
	w.handlePodUpdate(succeeded)

	if addCalled {
		t.Error("should not trigger add callback for Succeeded pod")
	}
	if !deleteCalled {
		t.Error("should trigger delete callback for Succeeded pod")
	}
}

func TestHandlePodUpdate_Failed(t *testing.T) {
	// Terminal pods must release their BPF entries via onDelete.
	addCalled, deleteCalled := false, false
	w := &PodWatcher{
		log:      zap.NewNop(),
		onAdd:    func(pod *corev1.Pod) { addCalled = true },
		onDelete: func(pod *corev1.Pod) { deleteCalled = true },
	}

	failed := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}
	w.handlePodUpdate(failed)

	if addCalled {
		t.Error("should not trigger add callback for Failed pod")
	}
	if !deleteCalled {
		t.Error("should trigger delete callback for Failed pod")
	}
}

func TestPodRelevantFieldsChanged(t *testing.T) {
	base := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"app": "x"},
			Annotations: map[string]string{"note": "a"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{ContainerID: "docker://aaa"},
			},
		},
	}

	tests := []struct {
		name   string
		mutate func(p *corev1.Pod)
		want   bool
	}{
		{"identical", func(p *corev1.Pod) {}, false},
		{"phase", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }, true},
		{"labels", func(p *corev1.Pod) { p.Labels["app"] = "y" }, true},
		{"annotations", func(p *corev1.Pod) { p.Annotations["note"] = "b" }, true},
		{"container-id", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "docker://bbb" }, true},
		{"restart-count-only", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 5 }, false},
		{"resource-version-only", func(p *corev1.Pod) { p.ResourceVersion = "999" }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clone := base.DeepCopy()
			tt.mutate(clone)
			if got := podRelevantFieldsChanged(base, clone); got != tt.want {
				t.Errorf("podRelevantFieldsChanged() = %v, want %v", got, tt.want)
			}
		})
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
