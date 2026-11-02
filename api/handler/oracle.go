package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"dbgold/store"
	"github.com/gin-gonic/gin"
	goora "github.com/sijms/go-ora/v2"
)

const oracleSchemaTimeout = 15 * time.Second

// ALL_USERS and this explicit filter also work on 11g (no ORACLE_MAINTAINED).
func oracleSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT USERNAME FROM ALL_USERS WHERE USERNAME = USER OR USERNAME NOT IN (
 'SYS','SYSTEM','OUTLN','DBSNMP','SYSMAN','APPQOSSYS','ANONYMOUS','XDB','CTXSYS','MDSYS','ORDSYS','ORDDATA','ORDPLUGINS',
 'SI_INFORMTN_SCHEMA','WMSYS','EXFSYS','DIP','ORACLE_OCM','OLAPSYS','XS$NULL','FLOWS_FILES','APEX_PUBLIC_USER',
 'AUDSYS','GSMADMIN_INTERNAL','GSMCATUSER','GSMUSER','OJVMSYS','REMOTE_SCHEDULER_AGENT','SYSBACKUP','SYSDG','SYSKM','SYSRAC') ORDER BY USERNAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
func listOracleSchemas(c *gin.Context, conn *store.Connection) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), oracleSchemaTimeout)
	defer cancel()
	// Bound socket I/O as well as the request context: go-ora otherwise uses
	// a 120-second network timeout. Keep this local to metadata requests.
	dsn := goora.BuildUrl(conn.Host, conn.Port, conn.Database, conn.Username, conn.Password,
		map[string]string{"CONNECTION TIMEOUT": strconv.Itoa(int(oracleSchemaTimeout / time.Second))})
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		c.Error(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()
	schemas, err := oracleSchemas(ctx, db)
	if err != nil {
		c.Error(err)
		var networkError net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
			(errors.As(err, &networkError) && networkError.Timeout()) {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "读取 Oracle Schema 列表超时，请检查数据库网络连接和数据字典查询耗时"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("读取 Oracle Schema 列表失败: %v", err)})
		return
	}
	c.JSON(http.StatusOK, schemas)
}
