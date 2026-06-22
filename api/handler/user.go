package handler

import (
	"dbgold/middleware"
	"dbgold/store"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func ListUsers(c *gin.Context) {
	users, err := store.ListUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, users)
}

func CreateUser(c *gin.Context) {
	var body struct {
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required,min=6"`
		Role     string `json:"role" binding:"required,oneof=admin user"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	u, err := store.CreateUser(body.Username, body.Password, body.Role)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "username already exists"})
		return
	}
	c.JSON(http.StatusCreated, u)
}

func UpdateUser(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body struct {
		Enabled  *bool  `json:"enabled"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 防呆：禁用账号时的边界校验
	if body.Enabled != nil && !*body.Enabled {
		target, err := store.GetUserByID(uint(id))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		// 不能禁用自己，避免误操作把自己锁死
		if uint(id) == middleware.GetCurrentUserID(c) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不能禁用当前登录的账号"})
			return
		}
		// 不能禁用最后一个启用的 admin，否则系统将无管理员
		if target.Role == "admin" && target.Enabled {
			count, err := store.CountEnabledAdmins()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if count <= 1 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "不能禁用最后一个管理员账号"})
				return
			}
		}
	}

	updates := map[string]any{}
	if body.Enabled != nil {
		updates["enabled"] = *body.Enabled
	}
	if body.Password != "" {
		hashed, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}
		updates["password"] = string(hashed)
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "nothing to update"})
		return
	}
	if err := store.UpdateUser(uint(id), updates); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "updated"})
}
