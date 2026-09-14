package requestid

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	gc := c.Gin()
	state := p.state.Load()
	if state == nil {
		web.AbortProblem(gc, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
		return nil
	}

	id := ""
	if state.trustIncoming {
		id = validIncomingID(c.Request().Header.Values(state.header), state.maxLength)
	}
	if id == "" {
		var err error
		id, err = generateID()
		if err != nil {
			web.AbortProblem(gc, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
			return nil
		}
	}

	c.Set(ginContextKey, id)
	newCtx := withRequestID(c.Request().Context(), id)
	c.SetContext(newCtx)
	c.SetHeader(state.header, id)
	c.Next()
	return nil
}

func validIncomingID(values []string, maxLength int) string {
	if len(values) != 1 {
		return ""
	}
	value := values[0]
	if len(value) < 1 || len(value) > maxLength {
		return ""
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == ':' {
			continue
		}
		return ""
	}
	return value
}

func generateID() (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
