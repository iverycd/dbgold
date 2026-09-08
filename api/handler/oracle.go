package handler

import (
	"context"
	"database/sql"
	"net/http"

	"dbgold/store"
	"github.com/gin-gonic/gin"
)

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
	return result, rows.Err()
}
func listOracleSchemas(c *gin.Context, conn *store.Connection) {
	db, err := sql.Open("oracle", buildDSN(conn))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer db.Close()
	schemas, err := oracleSchemas(c.Request.Context(), db)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, schemas)
}
