package main

import "sync"

// queue holds the repositories waiting for a sync, each at most once, served by one worker: a
// burst of webhooks for one repository costs one sync, and two syncs never race on a mirror or on
// the node's storage.
type queue struct {
	mu      sync.Mutex
	order   []int64
	pending map[int64]bool
	wake    chan struct{}
}

func newQueue() *queue {
	return &queue{pending: map[int64]bool{}, wake: make(chan struct{}, 1)}
}

func (q *queue) add(id int64) {
	q.mu.Lock()
	if !q.pending[id] {
		q.pending[id] = true
		q.order = append(q.order, id)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// next pops the oldest repository, or reports false when the queue is empty.
func (q *queue) next() (int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return 0, false
	}
	id := q.order[0]
	q.order = q.order[1:]
	delete(q.pending, id)
	return id, true
}

func (q *queue) run(work func(id int64)) {
	for {
		id, ok := q.next()
		if !ok {
			<-q.wake
			continue
		}
		work(id)
	}
}
