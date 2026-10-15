package handler

import (
	"time"

	"dbgold/datamigrate"
	"dbgold/store"
	"github.com/gin-gonic/gin"
)

const dataMigrationHeartbeatInterval = 15 * time.Second

// Call only after validating the token and task ownership. Comments flush the
// connection without becoming migration log messages in EventSource clients.
func streamDataMigrationLogs(c *gin.Context, dbJob *store.DataMigrationJob, job *datamigrate.Job, heartbeatInterval time.Duration) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	if !writeDataMigrationComment(c, "connected") {
		return
	}

	if job == nil {
		// The worker may have finished before the client opened the stream.
		if dbJob.Summary != "" {
			prefix := "[ERROR] "
			if dbJob.Status == "cancelled" {
				prefix = "[WARN] "
			}
			c.SSEvent("message", prefix+dbJob.Summary)
		}
		c.SSEvent("message", "[STREAM_END]")
		c.Writer.Flush()
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if !writeDataMigrationComment(c, "keep-alive") {
				return
			}
		case message, ok := <-job.LogCh:
			if !ok {
				c.SSEvent("message", "[STREAM_END]")
				c.Writer.Flush()
				return
			}
			c.SSEvent("message", message)
			c.Writer.Flush()
		}
	}
}

func writeDataMigrationComment(c *gin.Context, comment string) bool {
	if _, err := c.Writer.WriteString(": " + comment + "\n\n"); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}
