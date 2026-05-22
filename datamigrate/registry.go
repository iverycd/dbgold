// datamigrate/registry.go
package datamigrate

import (
	"context"
	"sync"
)

// Job 表示一个运行中的迁移任务
type Job struct {
	LogCh  chan string      // SSE 日志 channel（buffered，容量 512）
	Cancel context.CancelFunc
}

// JobRegistry 管理运行中的迁移任务，线程安全
type JobRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*Job
}

var Registry = &JobRegistry{jobs: make(map[string]*Job)}

// Register 注册一个新任务，返回 Job
func (r *JobRegistry) Register(jobID string, cancel context.CancelFunc) *Job {
	job := &Job{
		LogCh:  make(chan string, 512),
		Cancel: cancel,
	}
	r.mu.Lock()
	r.jobs[jobID] = job
	r.mu.Unlock()
	return job
}

// Get 获取运行中的任务，不存在返回 nil
func (r *JobRegistry) Get(jobID string) *Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.jobs[jobID]
}

// Remove 任务结束后从注册表移除
func (r *JobRegistry) Remove(jobID string) {
	r.mu.Lock()
	delete(r.jobs, jobID)
	r.mu.Unlock()
}
