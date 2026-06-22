package middleware

import (
	"dbgold/store"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

const userIDKey = "userID"
const roleKey = "role"

type Claims struct {
	UserID uint   `json:"user_id"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

var jwtSecret []byte

func SetJWTSecret(secret string) {
	jwtSecret = []byte(secret)
}

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		tokenStr := strings.TrimPrefix(header, "Bearer ")

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
			return jwtSecret, nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		user, err := store.GetUserByID(claims.UserID)
		if err != nil || !user.Enabled {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "user not found or disabled"})
			return
		}

		c.Set(userIDKey, claims.UserID)
		c.Set(roleKey, claims.Role)
		c.Next()
	}
}

func AdminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, _ := c.Get(roleKey)
		if role != "admin" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin required"})
			return
		}
		c.Next()
	}
}

func GetCurrentUserID(c *gin.Context) uint {
	id, _ := c.Get(userIDKey)
	return id.(uint)
}

// IsAdmin 返回当前请求用户是否为 admin 角色。
func IsAdmin(c *gin.Context) bool {
	role, _ := c.Get(roleKey)
	return role == "admin"
}

// ValidateTokenString 验证 JWT token 字符串并返回 claims，供需要手动校验 token 的 handler 使用（例如 SSE 端点）。
func ValidateTokenString(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	user, err := store.GetUserByID(claims.UserID)
	if err != nil || !user.Enabled {
		return nil, fmt.Errorf("user not found or disabled")
	}
	return claims, nil
}
