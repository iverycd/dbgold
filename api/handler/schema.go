package handler

import (
	"dbgold/driver"
	"dbgold/middleware"
	"dbgold/schema"
	"dbgold/store"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

type extractRequest struct {
	ConnectionID uint   `json:"connection_id" binding:"required"`
	Database     string `json:"database" binding:"required"`
}

func ExtractSchema(c *gin.Context) {
	var body extractRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, err := store.GetConnectionOwned(body.ConnectionID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractSchema(body.Database)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func ExtractFullSchema(c *gin.Context) {
	var body extractRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	conn, err := store.GetConnectionOwned(body.ConnectionID, middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractFullObjects(body.Database)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func ParseDDLFile(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s, err := schema.ParseDDL(string(data))
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}

func ExportDDL(c *gin.Context) {
	idStr := c.Query("connection_id")
	dbName := c.Query("database")
	if idStr == "" || dbName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connection_id and database required"})
		return
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connection_id"})
		return
	}
	conn, err := store.GetConnectionOwned(uint(id), middleware.GetCurrentUserID(c), middleware.IsAdmin(c))
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
	s, err := d.ExtractFullObjects(dbName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, s)
}
