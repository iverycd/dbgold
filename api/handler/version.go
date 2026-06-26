package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 以下变量由 deploy.sh 编译时通过 -ldflags -X 注入。
// 本地直接 go build / go run 时保持默认值(开发态)。
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

// GetVersion 返回系统版本信息,公开端点(登录页也能用)。
func GetVersion(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"version":    Version,
		"git_commit": GitCommit,
		"build_time": BuildTime,
	})
}
