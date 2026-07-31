package cel

import (
	"sync"
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

// TestCache_Singleflight verifies that concurrent misses for the same
// expression share one compile: the inflight map must be empty after
// the burst and exactly one entry must be cached.
func TestCache_Singleflight(t *testing.T) {
	c := NewCache(16)

	const expr = "1 < 2"
	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Evaluate(expr, nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	c.mu.RLock()
	inflight := len(c.inflight)
	total := len(c.entries)
	c.mu.RUnlock()

	if inflight != 0 {
		t.Errorf("inflight map should be drained after compile, got %d pending", inflight)
	}
	if total != 1 {
		t.Errorf("cache should hold exactly one entry, got %d", total)
	}
}

// TestCache_SingleflightError verifies a failed compile is not cached
// and later callers retry instead of hanging on a dead inflight entry.
func TestCache_SingleflightError(t *testing.T) {
	c := NewCache(16)

	const expr = "this is not CEL"
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Evaluate(expr, nil)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("goroutine %d: expected compile error", i)
		}
	}

	c.mu.RLock()
	inflight := len(c.inflight)
	cached := c.peekLocked(expr)
	c.mu.RUnlock()
	if inflight != 0 {
		t.Errorf("inflight map should be drained after failed compile, got %d", inflight)
	}
	if cached {
		t.Error("failed compile must not be cached")
	}
}

// peekLocked reports whether expr is cached; caller must hold c.mu.
func (c *Cache) peekLocked(expr string) bool {
	_, ok := c.entries[expr]
	return ok
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
