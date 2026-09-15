package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func limitRequestBody(maximum int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maximum)
		if c.Request.ContentLength > maximum {
			AbortProblem(newCtx(c), NewProblem(http.StatusRequestEntityTooLarge, "request_body_too_large"))
			return
		}
		c.Next()
	}
}
