// Package proxy contains small, dependency-free helpers for configuring and
// rotating outbound proxies.
package proxy

import "sync/atomic"

// Pool is a lock-free round-robin selector over a fixed set of items.
//
// The selector is intentionally per-request rather than per-connection: the
// upstream API is stateless from this proxy's point of view, so spreading
// independent requests is safer than pinning a connection. Each item is
// expected to own its own transport (and therefore its own connection pool),
// so rotation never changes the proxy mid-connection.
type Pool[T any] struct {
	items []T
	next  atomic.Uint64
}

// NewPool builds a round-robin pool. The slice is copied so later mutations by
// the caller cannot race with requests in flight.
func NewPool[T any](items []T) *Pool[T] {
	cp := make([]T, len(items))
	copy(cp, items)
	return &Pool[T]{items: cp}
}

// Len reports how many items the pool holds.
func (p *Pool[T]) Len() int {
	if p == nil {
		return 0
	}
	return len(p.items)
}

// First returns the first item, or the zero value when the pool is empty.
func (p *Pool[T]) First() T {
	if p == nil || len(p.items) == 0 {
		var zero T
		return zero
	}
	return p.items[0]
}

// Next returns the next item in round-robin order. The second return value is
// false only when the pool is empty.
func (p *Pool[T]) Next() (T, bool) {
	if p == nil || len(p.items) == 0 {
		var zero T
		return zero, false
	}
	// Add(1)-1 starts at index 0 so the first request uses the first configured
	// proxy, matching the order operators write in config.json.
	idx := (p.next.Add(1) - 1) % uint64(len(p.items))
	return p.items[idx], true
}
