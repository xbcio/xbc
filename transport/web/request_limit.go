package web

import (
	"context"
	"net/http"
)

func limitRequestBody(maximum int64) Handler {
	return func(_ context.Context, c *Ctx) error {
		request := c.Request()
		if request == nil || request.Body == nil {
			return nil
		}
		request.Body = http.MaxBytesReader(c.Writer(), request.Body, maximum)
		if request.ContentLength > maximum {
			AbortProblem(c, NewProblem(http.StatusRequestEntityTooLarge, "request_body_too_large"))
			return nil
		}
		return nil
	}
}
