// CEL evaluation cache (reused from Lymphocyte)
//
// LRU cache for precompiled CEL expressions.
// Evaluates scheduling policy conditions like:
//
//	"signal.podCPU > context.cpuLimit * 0.8"
//
// Architecture: hashmap + atomic clock for LRU ordering. Lookups are
// O(1); eviction scans linearly, which is fine for the small capacities
// used here (policy CEL expressions number in the tens, not thousands).
// Read path uses RLock (high concurrency), write path uses Lock.
// Original source: github.com/fengrru/lymphocyte/internal/cel/cache.go
package cel

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/google/cel-go/cel"
)

var globalClock atomic.Uint64

// Cache provides an LRU cache of precompiled CEL programs.
type Cache struct {
	mu       sync.RWMutex
	capacity int
	entries  map[string]*cacheEntry
	// inflight deduplicates concurrent compiles of the same expression
	// (singleflight), so a thundering herd of updates doesn't compile
	// the same source N times.
	inflight map[string]*compileCall
}

type cacheEntry struct {
	// clock is updated on every cache hit while only the read lock is
	// held, so it must be atomic to stay race-free.
	clock   atomic.Uint64
	program cel.Program
}

// compileCall is a single in-flight compile result shared by all
// callers waiting on the same expression.
type compileCall struct {
	done chan struct{}
	prog cel.Program
	err  error
}

// NewCache creates a CEL program cache with the given capacity.
func NewCache(capacity int) *Cache {
	return &Cache{
		capacity: capacity,
		entries:  make(map[string]*cacheEntry, capacity),
		inflight: make(map[string]*compileCall),
	}
}

// Evaluate checks if the CEL expression evaluates to true.
// Expressions are cached by their source string.
func (c *Cache) Evaluate(expr string, vars map[string]interface{}) (bool, error) {
	prog, err := c.getOrCompile(expr)
	if err != nil {
		return false, err
	}
	return c.runProgram(prog, vars)
}

// getOrCompile returns a cached program or compiles and caches a new one.
func (c *Cache) getOrCompile(expr string) (cel.Program, error) {
	// Fast path: RLock for concurrent reads.
	c.mu.RLock()
	if entry, ok := c.entries[expr]; ok {
		entry.clock.Store(globalClock.Add(1))
		prog := entry.program
		c.mu.RUnlock()
		return prog, nil
	}
	c.mu.RUnlock()

	// Slow path: compile with no lock held. Concurrent callers for the
	// same expression share one compile via the inflight map instead of
	// each compiling their own copy.
	c.mu.Lock()
	if call, ok := c.inflight[expr]; ok {
		c.mu.Unlock()
		<-call.done
		return call.prog, call.err
	}
	call := &compileCall{done: make(chan struct{})}
	c.inflight[expr] = call
	c.mu.Unlock()

	call.prog, call.err = c.compile(expr)
	if call.err != nil {
		call.err = fmt.Errorf("compile %q: %w", expr, call.err)
	}
	close(call.done)

	// Acquire write lock to insert into cache.
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, expr)

	if call.err != nil {
		return nil, call.err
	}

	// Double-check: another goroutine may have inserted the same expr
	// while we were compiling.
	if entry, ok := c.entries[expr]; ok {
		entry.clock.Store(globalClock.Add(1))
		return entry.program, nil
	}

	c.evictIfNeeded()

	entry := &cacheEntry{program: call.prog}
	entry.clock.Store(globalClock.Add(1))
	c.entries[expr] = entry

	return call.prog, nil
}

func (c *Cache) runProgram(prog cel.Program, vars map[string]interface{}) (bool, error) {
	out, _, err := prog.Eval(vars)
	if err != nil {
		return false, fmt.Errorf("eval: %w", err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("expression must return bool, got %T", out.Value())
	}
	return b, nil
}

// peek reports whether expr is already cached, without compiling it.
// Used by tests to inspect eviction behavior.
func (c *Cache) peek(expr string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.entries[expr]
	return ok
}

func (c *Cache) compile(expr string) (cel.Program, error) {
	env, err := cel.NewEnv(
		cel.Variable("signal", cel.MapType(cel.StringType, cel.AnyType)),
		cel.Variable("context", cel.MapType(cel.StringType, cel.AnyType)),
	)
	if err != nil {
		return nil, err
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	prog, err := env.Program(ast)
	if err != nil {
		return nil, err
	}
	return prog, nil
}

func (c *Cache) evictIfNeeded() {
	for len(c.entries) >= c.capacity {
		var oldestKey string
		oldestClock := ^uint64(0)
		for k, e := range c.entries {
			if clk := e.clock.Load(); clk < oldestClock {
				oldestClock = clk
				oldestKey = k
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}
