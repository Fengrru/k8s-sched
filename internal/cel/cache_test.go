package cel

import (
	"testing"
)

func TestCache_Evaluate_Simple(t *testing.T) {
	c := NewCache(16)

	ok, err := c.Evaluate("true", nil)
	if err != nil {
		t.Fatalf("eval true: %v", err)
	}
	if !ok {
		t.Fatal("true should evaluate to true")
	}
}

func TestCache_Evaluate_WithVars(t *testing.T) {
	c := NewCache(16)

	vars := map[string]interface{}{
		"signal": map[string]interface{}{
			"podCPU": 0.95,
		},
		"context": map[string]interface{}{
			"cpuLimit": 1.0,
		},
	}

	expr := "signal.podCPU > context.cpuLimit * 0.8"
	ok, err := c.Evaluate(expr, vars)
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	if !ok {
		t.Fatalf("%q should be true: 0.95 > 0.8", expr)
	}

	vars["signal"].(map[string]interface{})["podCPU"] = 0.5
	ok, err = c.Evaluate(expr, vars)
	if err != nil {
		t.Fatalf("eval %q (low cpu): %v", expr, err)
	}
	if ok {
		t.Fatalf("%q should be false: 0.5 <= 0.8", expr)
	}
}

func TestCache_Evaluate_InvalidExpr(t *testing.T) {
	c := NewCache(16)

	_, err := c.Evaluate("this is not CEL", nil)
	if err == nil {
		t.Fatal("invalid expression should return an error")
	}
}

func TestCache_Evaluate_CacheHit(t *testing.T) {
	c := NewCache(2)

	_, err := c.Evaluate("true", nil)
	if err != nil {
		t.Fatal(err)
	}

	c.mu.RLock()
	_, ok := c.entries["true"]
	c.mu.RUnlock()

	if !ok {
		t.Fatal("expression should be cached after first evaluation")
	}
}

func TestCache_Eviction(t *testing.T) {
	c := NewCache(2)

	c.Evaluate("1 < 2", nil)
	c.Evaluate("2 < 3", nil)
	c.Evaluate("3 < 4", nil)

	c.mu.RLock()
	count := len(c.entries)
	c.mu.RUnlock()

	if count != 2 {
		t.Fatalf("cache should evict oldest entry: want 2 items, got %d", count)
	}

	// "1 < 2" should have been evicted (it was oldest).
	ok := c.peek("1 < 2")
	if ok {
		t.Fatal("oldest entry '1 < 2' should have been evicted, but was still in cache")
	}

	// "2 < 3" and "3 < 4" should still be in cache.
	ok = c.peek("2 < 3")
	if !ok {
		t.Fatal("entry '2 < 3' should still be in cache")
	}
	ok = c.peek("3 < 4")
	if !ok {
		t.Fatal("entry '3 < 4' should still be in cache")
	}
}

func TestCache_Evaluate_ReturnType(t *testing.T) {
	c := NewCache(16)

	_, err := c.Evaluate("42", nil)
	if err == nil {
		t.Fatal("non-boolean expression should fail (CEL requires bool return)")
	}
}

func TestCache_Concurrency(t *testing.T) {
	c := NewCache(100)
	done := make(chan bool)

	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				c.Evaluate("true", nil)
			}
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}
}

// Helper to avoid modifying cache.go for tests.
func (c *Cache) evaluate(expr string, vars map[string]interface{}) (result, ok bool) {
	c.mu.RLock()
	entry, found := c.entries[expr]
	c.mu.RUnlock()
	if !found {
		return false, false
	}
	v, err := c.runProgram(entry.program, vars)
	return v, err == nil
}
