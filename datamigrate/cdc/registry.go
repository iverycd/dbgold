package cdc

import (
	"context"
	"sync"
)

type control struct {
	cancel context.CancelFunc
	action string
}
type JobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*control
}

var Registry = &JobRegistry{jobs: map[string]*control{}}

func (r *JobRegistry) Register(id string) (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[id]; ok {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.jobs[id] = &control{cancel: cancel}
	return ctx, nil
}
func (r *JobRegistry) Cancel(id, action string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		return false
	}
	j.action = action
	j.cancel()
	return true
}
func (r *JobRegistry) Remove(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	if j == nil {
		return ""
	}
	delete(r.jobs, id)
	return j.action
}
func (r *JobRegistry) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.jobs[id] != nil
}
