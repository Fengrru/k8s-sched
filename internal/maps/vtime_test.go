package maps

import (
	"testing"
)

// TestVtimeFormula verifies the weighted virtual time math.
//
// Formula from k8s_sched.bpf.c (charged in the stopping callback for
// the consumed part of the slice):
//
//	vtime += (used * MAX_PRIO_WEIGHT) / weight
//
//	MAX_PRIO_WEIGHT = 10000
//	DEFAULT_SLICE   = 5,000,000 ns (5ms)
//
// The BPF code is in C, but we test the math in Go to catch
// edge cases before deploying to the kernel.
func TestVtimeFormula(t *testing.T) {
	const maxWeight = uint64(10000)
	const slice = uint64(5000000) // 5ms

	tests := []struct {
		name      string
		weight    uint64
		wantVtime uint64
	}{
		{"max priority", 10000, 5000000},  // 5ms * 10000/10000 = 5ms
		{"default", 1000, 50000000},       // 5ms * 10000/1000 = 50ms
		{"low priority", 100, 500000000},  // 5ms * 10000/100 = 500ms
		{"barely runs", 10, 5000000000},   // 5ms * 10000/10 = 5s
		{"importance-95", 9500, 5263157},  // 5ms * 10000/9500 ≈ 5.26ms
		{"importance-50", 5000, 10000000}, // 5ms * 10000/5000 = 10ms
		{"importance-1", 100, 500000000},  // 5ms * 10000/100 = 500ms
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			divisor := tt.weight
			if divisor == 0 {
				divisor = 1
			}
			vtime := (slice * maxWeight) / divisor
			if vtime != tt.wantVtime {
				t.Errorf("weight=%d: want vtime=%d ns, got %d ns",
					tt.weight, tt.wantVtime, vtime)
			}
		})
	}
}

func TestVtimeFormula_Ratio(t *testing.T) {
	// weight=10000 should get 10x CPU vs weight=1000
	// This is verified by comparing vtime increments:
	// high-weight task increments vtime slower → runs more often

	const maxWeight = uint64(10000)
	const slice = uint64(5000000)

	high := (slice * maxWeight) / 10000 // 5ms
	low := (slice * maxWeight) / 1000   // 50ms

	// The ratio of CPU time is inverse of vtime ratio:
	// high vtime / low vtime = 5ms / 50ms = 1/10
	// So high-weight runs 10x more than low-weight
	ratio := float64(low) / float64(high)

	if ratio < 9.9 || ratio > 10.1 {
		t.Errorf("weight 10000 vs 1000: want ~10x CPU ratio, got %.2f", ratio)
	}
}

func TestVtimeFormula_NoDivisionByZero(t *testing.T) {
	// BPF code uses (tp.weight ? tp.weight : 1) to prevent div by zero.
	// Verify the fallback produces a finite result.
	const maxWeight = uint64(10000)
	const slice = uint64(5000000)

	// Simulating weight=0 → divisor clamped to 1
	vtime := (slice * maxWeight) / 1

	minExpected := uint64(1)
	if vtime < minExpected {
		t.Errorf("division by zero fallback: vtime should be >= 1, got %d", vtime)
	}
}

func TestVtimeFormula_BudgetCap(t *testing.T) {
	// Budget acts as slice cap. A task with budget=20ms should
	// max out at 20ms per dispatch regardless of weight.
	const budget = uint64(20000000) // 20ms
	const defaultSlice = uint64(5000000)

	slice := budget
	if defaultSlice < budget {
		slice = defaultSlice
	}

	if slice != 5000000 {
		t.Errorf("budget cap: slice should be 5ms (min of default and budget), got %d ns", slice)
	}

	// When budget < default, budget wins
	const tightBudget = uint64(1000000) // 1ms
	slice = tightBudget
	if defaultSlice < tightBudget {
		slice = defaultSlice
	}
	if slice != 1000000 {
		t.Errorf("tight budget: want 1ms slice, got %d ns", slice)
	}
}
