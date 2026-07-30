package maps

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BenchmarkExtractSchedulingParams(b *testing.B) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				annotationWeight: "8000",
				annotationBudget: "20000",
			},
		},
	}

	b.Run("with_annotations", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ExtractSchedulingParams(pod)
		}
	})

	b.Run("no_annotations", func(b *testing.B) {
		emptyPod := &corev1.Pod{}
		for i := 0; i < b.N; i++ {
			ExtractSchedulingParams(emptyPod)
		}
	})
}

func BenchmarkParseAnnotationWeight(b *testing.B) {
	ann := map[string]string{
		annotationImportance: "80",
	}

	b.Run("hit", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ParseAnnotationWeight(ann)
		}
	})

	b.Run("miss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			ParseAnnotationWeight(nil)
		}
	})
}

func BenchmarkParseCgroupProcs(b *testing.B) {
	data := []byte("12345\n67890\n11111\n")

	for i := 0; i < b.N; i++ {
		parseCgroupProcs(data)
	}
}

func BenchmarkParseCgroupProcs_Large(b *testing.B) {
	data := make([]byte, 0, 5000)
	for i := int32(1); i <= 500; i++ {
		data = append(data, []byte("10000\n")...)
	}

	for i := 0; i < b.N; i++ {
		parseCgroupProcs(data)
	}
}
