package cel

import (
	"testing"
)

func BenchmarkCache_Evaluate(b *testing.B) {
	c := NewCache(256)
	vars := map[string]interface{}{
		"signal": map[string]interface{}{
			"podName":       "test-pod",
			"podNamespace":  "default",
			"podCPURequest": 0.5,
			"podCPULimit":   1.0,
			"nodeName":      "node-1",
		},
		"context": map[string]interface{}{
			"policyName": "test-policy",
			"weight":     5000,
			"budgetUs":   20000,
		},
	}

	b.Run("simple_expr", func(b *testing.B) {
		expr := "signal.podCPURequest > 0.3"
		for i := 0; i < b.N; i++ {
			c.Evaluate(expr, vars)
		}
	})

	b.Run("complex_expr", func(b *testing.B) {
		expr := "signal.podCPURequest > context.budgetUs * 0.0001 && signal.podCPULimit < 4.0"
		for i := 0; i < b.N; i++ {
			c.Evaluate(expr, vars)
		}
	})

	b.Run("cache_hit", func(b *testing.B) {
		expr := "signal.podName == 'test-pod'"
		// Warm up cache
		c.Evaluate(expr, vars)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			c.Evaluate(expr, vars)
		}
	})
}

func BenchmarkCache_ConcurrentRead(b *testing.B) {
	c := NewCache(256)
	expr := "signal.podCPURequest > 0.3"
	vars := map[string]interface{}{
		"signal": map[string]interface{}{
			"podCPURequest": 0.5,
		},
	}
	c.Evaluate(expr, vars) // warm up

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Evaluate(expr, vars)
		}
	})
}
