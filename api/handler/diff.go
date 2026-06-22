package handler

import (
	"dbgold/diff"
	"dbgold/driver"
	"dbgold/middleware"
	"dbgold/schema"
	"dbgold/store"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type diffRequest struct {
	SrcConnectionID uint           `json:"src_connection_id"`
	SrcDatabase     string         `json:"src_database"`
	SrcSchema       *schema.Schema `json:"src_schema"`

	DstConnectionID uint           `json:"dst_connection_id"`
	DstDatabase     string         `json:"dst_database"`
	DstSchema       *schema.Schema `json:"dst_schema"`
}

func DiffSchemas(c *gin.Context) {
	var body diffRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	srcSchema, err := resolveSchema(c, body.SrcConnectionID, body.SrcDatabase, body.SrcSchema)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "src: " + err.Error()})
		return
	}
	dstSchema, err := resolveSchema(c, body.DstConnectionID, body.DstDatabase, body.DstSchema)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dst: " + err.Error()})
		return
	}

	result := diff.Compare(srcSchema, dstSchema)
	c.JSON(http.StatusOK, result)
}

func resolveSchema(c *gin.Context, connID uint, dbName string, inline *schema.Schema) (*schema.Schema, error) {
	if inline != nil {
		return inline, nil
	}
	if connID == 0 || dbName == "" {
		return nil, fmt.Errorf("connection_id and database are required when schema is not provided inline")
	}
	conn, err := store.GetConnectionOwned(connID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
	if err != nil {
		return nil, fmt.Errorf("connection not found")
	}
	d, err := driver.NewDriver(conn.DBType)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err := d.Connect(buildDSN(conn)); err != nil {
		return nil, err
	}
	return d.ExtractSchema(dbName)
}
