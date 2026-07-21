package handler

import (
	"fmt"
	"strconv"
	"strings"

	"dbgold/store"
	"github.com/gin-gonic/gin"
)

var dataMigrationListStatuses = map[string]struct{}{
	"running": {}, "done": {}, "failed": {}, "cancelled": {},
}

var incrementalListStatuses = map[string]struct{}{
	"active": {}, "attention": {}, "completed": {}, "aborted": {},
}

func parseJobListFilter(c *gin.Context, allowedStatuses map[string]struct{}, allowOrigin bool) (store.JobListFilter, error) {
	filter := store.JobListFilter{Page: 1, PageSize: 20, Origin: "all"}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return filter, fmt.Errorf("page 必须是大于等于 1 的整数")
		}
		filter.Page = page
	}
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		pageSize, err := strconv.Atoi(raw)
		if err != nil || (pageSize != 20 && pageSize != 50 && pageSize != 100) {
			return filter, fmt.Errorf("page_size 仅支持 20、50 或 100")
		}
		filter.PageSize = pageSize
	}
	filter.Keyword = strings.TrimSpace(c.Query("keyword"))
	filter.Status = strings.TrimSpace(c.Query("status"))
	if filter.Status != "" {
		if _, ok := allowedStatuses[filter.Status]; !ok {
			return filter, fmt.Errorf("不支持的 status: %s", filter.Status)
		}
	}
	if allowOrigin {
		filter.Origin = strings.TrimSpace(c.DefaultQuery("origin", "all"))
		if filter.Origin != "all" && filter.Origin != "single" && filter.Origin != "batch" {
			return filter, fmt.Errorf("origin 仅支持 all、single 或 batch")
		}
	}
	return filter, nil
}
