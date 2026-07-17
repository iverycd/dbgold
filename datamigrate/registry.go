// datamigrate/registry.go
package datamigrate

import (
	"context"
	"sync"
	"sync/atomic"
)

const jobLogBufferSize = 4096

// Job 表示一个运行中的迁移任务
type Job struct {
	LogCh  chan string // SSE/持久化日志 channel（buffered，容量 4096）
	Cancel context.CancelFunc

	droppedLogs atomic.Uint64
}

// DroppedLogCount 返回因为日志 channel 已满而被丢弃的日志行数。
// 该计数可在迁移运行期间并发读取。
func (j *Job) DroppedLogCount() uint64 {
	if j == nil {
		return 0
	}
	return j.droppedLogs.Load()
}

func (j *Job) recordDroppedLog() {
	if j != nil {
		j.droppedLogs.Add(1)
	}
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
		LogCh:  make(chan string, jobLogBufferSize),
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
