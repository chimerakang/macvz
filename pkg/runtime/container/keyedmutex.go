package container

import "sync"

// keyedMutex serializes operations that share a key (a workload ID) while
// letting operations on distinct keys proceed concurrently. This gives the
// Driver per-workload mutual exclusion without a global lock, so the provider
// can act on many micro-VMs at once but never races two ops on the same one.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// lock acquires the mutex for key and returns its unlock func. Entries persist
// for the process lifetime; the set is bounded by the number of workloads, so
// no reaping is needed in P1.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*sync.Mutex)
	}
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}
