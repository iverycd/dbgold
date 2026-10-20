package datamigrate

import (
	"context"
	"sync"

	"dbgold/datamigrate/source"
)

type sourceTableDDLResult struct {
	info *source.TableDDLInfo
	err  error
}

// prewarmSourceTableDDL overlaps only independent source metadata reads. The
// caller consumes all results, including errors and nil metadata, in table order
// so that preparation does not introduce retries. A nil map selects the existing
// serial path. DDL generation, target mappings and persistence remain serial.
func (m *Migrator) prewarmSourceTableDDL(ctx context.Context, tables []string, progress func(done, total int)) (map[string]sourceTableDDLResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, ok := m.reader.(interface{ MetadataReadConcurrency() int })
	if !ok {
		return nil, nil
	}
	parallel := min(4, reader.MetadataReadConcurrency())
	if parallel <= 1 {
		return nil, nil
	}
	excluded := make(map[string]bool)
	for _, object := range m.cfg.ExcludedObjects {
		if object.Kind == "table" {
			excluded[object.Name] = true
		}
	}
	selected := make([]string, 0, len(tables))
	seen := make(map[string]bool, len(tables))
	for _, table := range tables {
		if !seen[table] && !excluded[table] {
			seen[table] = true
			selected = append(selected, table)
		}
	}
	parallel = min(parallel, len(selected))
	if parallel <= 1 {
		return nil, nil
	}
	if progress != nil {
		progress(0, len(selected))
	}
	results := make([]sourceTableDDLResult, len(selected))
	jobs := make(chan int)
	completed := make(chan int, parallel)
	dispatchDone := make(chan struct{})
	var workers sync.WaitGroup
	for range parallel {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				info, err := m.reader.GetTableDDLInfo(ctx, selected[index])
				if m.stopOracleDamengTask(ctx, err) {
					return
				}
				results[index] = sourceTableDDLResult{info: info, err: err}
				completed <- index
			}
		}()
	}
	go func() {
		defer close(dispatchDone)
		defer close(jobs)
		for index := range selected {
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(completed)
	}()
	done := 0
	for range completed {
		done++
		if progress != nil {
			progress(done, len(selected))
		}
	}
	<-dispatchDone
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared := make(map[string]sourceTableDDLResult, len(selected))
	m.sourceDDLCache.mu.Lock()
	defer m.sourceDDLCache.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for index, table := range selected {
		result := results[index]
		prepared[table] = sourceTableDDLResult{info: cloneSourceTableDDLInfo(result.info), err: result.err}
		if result.err == nil && result.info != nil && m.sourceDDLCache.tables != nil {
			m.sourceDDLCache.tables[table] = cloneSourceTableDDLInfo(result.info)
		}
	}
	return prepared, nil
}
