package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// ipLimiter 记录单个 IP 的令牌桶及最近访问时间（用于空闲回收）。
type ipLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// ipRateLimiter 按客户端 IP 维护独立令牌桶的限流器。
// 纯内存实现，适用于单实例部署；后台定时回收长时间空闲的 IP，避免内存无限增长。
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipLimiter
	rate    rate.Limit
	burst   int
}

func newIPRateLimiter(r rate.Limit, burst int) *ipRateLimiter {
	l := &ipRateLimiter{
		buckets: make(map[string]*ipLimiter),
		rate:    r,
		burst:   burst,
	}
	go l.cleanupLoop()
	return l
}

// get 返回该 IP 的令牌桶，不存在则惰性创建。
func (l *ipRateLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &ipLimiter{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[ip] = b
	}
	b.lastSeen = time.Now()
	return b.limiter
}

// cleanupLoop 每 10 分钟清理一次超过 15 分钟未访问的 IP 记录。
func (l *ipRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-15 * time.Minute)
		l.mu.Lock()
		for ip, b := range l.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(l.buckets, ip)
			}
		}
		l.mu.Unlock()
	}
}

// RateLimit 构造一个按客户端 IP 限流的 gin 中间件。
// perMinute 为每分钟允许的稳态请求数，burst 为允许的瞬时突发数。
// 每次调用返回独立的限流器实例，因此不同端点的额度互不影响。
func RateLimit(perMinute int, burst int) gin.HandlerFunc {
	limiter := newIPRateLimiter(rate.Every(time.Minute/time.Duration(perMinute)), burst)
	return func(c *gin.Context) {
		if !limiter.get(c.ClientIP()).Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "请求过于频繁，请稍后再试",
			})
			return
		}
		c.Next()
	}
}
