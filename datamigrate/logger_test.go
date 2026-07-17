package datamigrate

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewLoggerKeepsNonBlockingDropBehavior(t *testing.T) {
	ch := make(chan string, 1)
	logger := NewLogger(ch)

	logger.Info("first")
	logger.Info("second")

	require.Len(t, ch, 1)
}

func TestMigratorLoggerCountsDroppedLines(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &Job{LogCh: make(chan string, 1), Cancel: cancel}
	migrator := NewMigrator(&mockReader{}, &mockWriter{}, job, Config{})

	migrator.log.Info("accepted")
	migrator.log.Warn("dropped")

	require.Equal(t, uint64(1), job.DroppedLogCount())
}

func TestDroppedLogCountIsConcurrentSafe(t *testing.T) {
	const sends = 1000
	job := &Job{LogCh: make(chan string, 1)}
	logger := newCountingLogger(job.LogCh, job.recordDroppedLog)

	var wg sync.WaitGroup
	wg.Add(sends)
	for i := 0; i < sends; i++ {
		go func() {
			defer wg.Done()
			logger.Info("line")
		}()
	}
	wg.Wait()

	require.Len(t, job.LogCh, 1)
	require.Equal(t, uint64(sends-1), job.DroppedLogCount())
}

func TestRegistryUsesExpandedLogBuffer(t *testing.T) {
	registry := &JobRegistry{jobs: make(map[string]*Job)}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	job := registry.Register("job", cancel)

	require.Equal(t, jobLogBufferSize, cap(job.LogCh))
	require.Zero(t, job.DroppedLogCount())
}
