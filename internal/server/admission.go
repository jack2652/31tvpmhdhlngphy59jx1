package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func requestAdmissionLimit(limit int) gin.HandlerFunc {
	if limit < 1 {
		limit = 64
	}
	active := make(chan struct{}, limit)
	return func(c *gin.Context) {
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
			c.Next()
		default:
			c.Header("Retry-After", "1")
			c.AbortWithStatus(http.StatusServiceUnavailable)
		}
	}
}
