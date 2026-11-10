package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"dbgold/store"
	"gitee.com/chunanyong/dm"
	"github.com/gin-gonic/gin"
)

const (
	damengSourceSchemaTimeout = 15 * time.Second
	damengSourceSchemaDial    = "dbgold-source-schema"
)

func init() {
	dm.RegisterDialContext(damengSourceSchemaDial, func(ctx context.Context, addr string) (net.Conn, error) {
		dialer := net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
		// The driver can read from the socket after QueryContext returns rows.
		// Apply the same request deadline to those reads and to connection cleanup.
		if deadline, ok := ctx.Deadline(); ok {
			if err := conn.SetDeadline(deadline); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
		return conn, nil
	})
}

func damengSourceSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("连接达梦: %w", err)
	}
	defer conn.Close()

	// The public synonym uses the SVI view when SVI is enabled.
	rows, err := conn.QueryContext(ctx, `SELECT NAME FROM SYSOBJECTS WHERE TYPE$='SCH' ORDER BY NAME`)
	var dmErr *dm.DmError
	if errors.As(err, &dmErr) {
		switch dmErr.ErrCode {
		case -2106, -5504, -5508: // missing object or missing SELECT privilege
			rows, err = conn.QueryContext(ctx, `SELECT DISTINCT OWNER FROM ALL_TABLES ORDER BY OWNER`)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("查询达梦源 Schema: %w", err)
	}
	defer rows.Close()

	schemas := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		schemas = append(schemas, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return schemas, nil
}

func listDaMengSourceSchemas(c *gin.Context, conn *store.Connection) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), damengSourceSchemaTimeout)
	defer cancel()
	// Keep the metadata operation on the request-bound socket even if driver
	// service configuration enables background reconnect or read splitting.
	dsn := buildDSN(conn) + "?dialName=" + damengSourceSchemaDial + "&driverReconnect=false&rwSeparate=0&doSwitch=0"
	db, err := sql.Open("dm", dsn)
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取达梦源 Schema 列表失败，请检查连接配置"})
		return
	}
	defer db.Close()

	schemas, err := damengSourceSchemas(ctx, db)
	if err != nil {
		c.Error(err)
		deadline, _ := ctx.Deadline()
		var networkError net.Error
		// DM wraps some socket errors without preserving their type. The socket
		// deadline can also expire before the context timer has fired.
		if !time.Now().Before(deadline) || errors.Is(err, context.DeadlineExceeded) ||
			(errors.As(err, &networkError) && networkError.Timeout()) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "读取达梦源 Schema 列表超时，请检查数据库网络连接和数据字典查询耗时"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "读取达梦源 Schema 列表失败，请检查连接配置和目录读取权限"})
		return
	}
	c.JSON(http.StatusOK, schemas)
}
