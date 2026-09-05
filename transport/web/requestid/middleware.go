package requestid

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) handle(c *gin.Context) {
	state := p.state.Load()
	if state == nil {
		web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
		return
	}

	id := ""
	if state.trustIncoming {
		id = validIncomingID(c.Request.Header.Values(state.header), state.maxLength)
	}
	if id == "" {
		var err error
		id, err = generateID()
		if err != nil {
			web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
			return
		}
	}

	c.Set(ginContextKey, id)
	c.Request = c.Request.WithContext(withRequestID(c.Request.Context(), id))
	c.Header(state.header, id)
	c.Next()
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
