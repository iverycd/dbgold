package handler

import (
	"dbgold/diff"
	"dbgold/driver"
	"dbgold/migrate"
	"dbgold/schema"
	"dbgold/store"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type diffMigrationRequest struct {
	SrcConnectionID uint           `json:"src_connection_id"`
	SrcDatabase     string         `json:"src_database"`
	SrcSchema       *schema.Schema `json:"src_schema"`
	DstConnectionID uint           `json:"dst_connection_id"`
	DstDatabase     string         `json:"dst_database"`
	DstSchema       *schema.Schema `json:"dst_schema"`
	DBType          string         `json:"db_type"` // dialect when both sides are inline
}

type fullMigrationRequest struct {
	SrcConnectionID uint   `json:"src_connection_id"`
	SrcDatabase     string `json:"src_database"`
	DstConnectionID uint   `json:"dst_connection_id" binding:"required"`
	DstDatabase     string `json:"dst_database" binding:"required"`
}

type selectiveMigrationRequest struct {
	ConnectionID uint                    `json:"connection_id" binding:"required"`
	Database     string                  `json:"database" binding:"required"`
	Objects      *schema.SelectedObjects `json:"objects" binding:"required"`
}

func RunDiffMigration(c *gin.Context) {
	var body diffMigrationRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	srcSchema, err := resolveSchema(body.SrcConnectionID, body.SrcDatabase, body.SrcSchema)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "src: " + err.Error()})
		return
	}
	dstSchema, err := resolveSchema(body.DstConnectionID, body.DstDatabase, body.DstSchema)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dst: " + err.Error()})
		return
	}

	diffResult := diff.Compare(srcSchema, dstSchema)

	var sqls []string
	if body.SrcConnectionID != 0 {
		conn, err := store.GetConnection(body.SrcConnectionID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "src connection not found"})
			return
		}
		d, err := driver.NewDriver(conn.DBType)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer d.Close()
		if err := d.Connect(buildDSN(conn)); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		sqls, err = d.GenerateDiffSQL(diffResult)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	} else {
		dbType := body.DBType
		if dbType == "" {
			dbType = "mysql"
		}
		sqls, err = generateDiffSQLByType(dbType, diffResult)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	sqlsJSON, _ := json.Marshal(sqls)
	m := &store.MigrationHistory{
		Type:          "diff",
		SrcConnID:     body.SrcConnectionID,
		SrcDatabase:   body.SrcDatabase,
		DstConnID:     body.DstConnectionID,
		DstDatabase:   body.DstDatabase,
		SQLStatements: string(sqlsJSON),
		Status:        "success",
	}
	_ = store.CreateMigration(m)

	c.JSON(http.StatusOK, gin.H{"id": m.ID, "sql_statements": sqls})
}

func RunFullMigration(c *gin.Context) {
	var body fullMigrationRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	dstConn, err := store.GetConnection(body.DstConnectionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "dst connection not found"})
		return
	}
	dstDriver, err := driver.NewDriver(dstConn.DBType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer dstDriver.Close()
	if err := dstDriver.Connect(buildDSN(dstConn)); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	dstFull, err := dstDriver.ExtractFullObjects(body.DstDatabase)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sqls, err := dstDriver.GenerateFullMigrationSQL(nil, dstFull)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sqlsJSON, _ := json.Marshal(sqls)
	m := &store.MigrationHistory{
		Type:          "full",
		SrcConnID:     body.SrcConnectionID,
		SrcDatabase:   body.SrcDatabase,
		DstConnID:     body.DstConnectionID,
		DstDatabase:   body.DstDatabase,
		SQLStatements: string(sqlsJSON),
		Status:        "success",
	}
	_ = store.CreateMigration(m)

	c.JSON(http.StatusOK, gin.H{"id": m.ID, "sql_statements": sqls})
}

func RunSelectiveMigration(c *gin.Context) {
	var body selectiveMigrationRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	conn, err := store.GetConnection(body.ConnectionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "connection not found"})
		return
	}
	d, err := driver.NewDriver(conn.DBType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	defer d.Close()
	if err := d.Connect(buildDSN(conn)); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	sqls, err := d.GenerateSelectiveSQL(body.Objects)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	sqlsJSON, _ := json.Marshal(sqls)
	m := &store.MigrationHistory{
		Type:          "selective",
		DstConnID:     body.ConnectionID,
		DstDatabase:   body.Database,
		SQLStatements: string(sqlsJSON),
		Status:        "success",
	}
	_ = store.CreateMigration(m)

	c.JSON(http.StatusOK, gin.H{"id": m.ID, "sql_statements": sqls})
}

func ListMigrations(c *gin.Context) {
	list, err := store.ListMigrations()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, list)
}

func GetMigration(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	m, err := store.GetMigration(uint(id))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, m)
}

func generateDiffSQLByType(dbType string, r *diff.Result) ([]string, error) {
	switch dbType {
	case "mysql":
		return migrate.MySQLGenerateDiffSQL(r, false)
	case "postgres":
		return migrate.PostgresGenerateDiffSQL(r, false)
	case "oracle":
		return migrate.OracleGenerateDiffSQL(r, false)
	case "sqlserver":
		return migrate.SQLServerGenerateDiffSQL(r, false)
	default:
		return migrate.MySQLGenerateDiffSQL(r, false)
	}
}
