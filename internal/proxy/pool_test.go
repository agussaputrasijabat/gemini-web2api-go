package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestPoolRoundRobin(t *testing.T) {
	p := NewPool([]string{"a", "b", "c"})
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, expected := range want {
		got, ok := p.Next()
		if !ok || got != expected {
			t.Fatalf("Next #%d = %q (ok=%v), want %q", i, got, ok, expected)
		}
	}
	if p.Len() != 3 {
		t.Fatalf("Len = %d, want 3", p.Len())
	}
	if p.First() != "a" {
		t.Fatalf("First = %q, want a", p.First())
	}
}

func TestPoolEmpty(t *testing.T) {
	var p *Pool[int]
	if got, ok := p.Next(); ok || got != 0 {
		t.Fatalf("nil pool Next = %d, %v; want zero, false", got, ok)
	}
	if p.Len() != 0 {
		t.Fatalf("nil pool Len = %d, want 0", p.Len())
	}
	if p.First() != 0 {
		t.Fatalf("nil pool First = %d, want 0", p.First())
	}

	empty := NewPool([]int{})
	if _, ok := empty.Next(); ok {
		t.Fatal("empty pool Next must report false")
	}
}

func TestPoolConcurrentRotationIsBalanced(t *testing.T) {
	const workers = 300
	p := NewPool([]int{0, 1, 2})
	var counts [3]atomic.Int64

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if item, ok := p.Next(); ok {
				counts[item].Add(1)
			}
		}()
	}
	wg.Wait()

	for i := range counts {
		if got := counts[i].Load(); got != workers/3 {
			t.Errorf("item %d selected %d times, want %d", i, got, workers/3)
		}
	}
}

func TestNewPoolCopiesItems(t *testing.T) {
	items := []string{"a", "b"}
	p := NewPool(items)
	items[0] = "mutated"
	if got := p.First(); got != "a" {
		t.Fatalf("First = %q, want the original a", got)
	}
}
