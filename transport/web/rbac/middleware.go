package rbac

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/gin-gonic/gin"

	businessrbac "github.com/xbcio/xbc/security/rbac"
	"github.com/xbcio/xbc/transport/web"
)

// RequireAll returns route/group middleware that requires every permission for
// the verified web.CurrentPrincipal subject.
func RequireAll(manager businessrbac.Manager, permissions ...businessrbac.Permission) gin.HandlerFunc {
	return require(manager, append([]businessrbac.Permission(nil), permissions...), func(ctx *gin.Context, subject string, permissions []businessrbac.Permission) (bool, error) {
		return manager.AuthorizeAll(ctx.Request.Context(), subject, permissions...)
	})
}

// RequireAny returns route/group middleware that requires at least one
// permission for the verified web.CurrentPrincipal subject.
func RequireAny(manager businessrbac.Manager, permissions ...businessrbac.Permission) gin.HandlerFunc {
	return require(manager, append([]businessrbac.Permission(nil), permissions...), func(ctx *gin.Context, subject string, permissions []businessrbac.Permission) (bool, error) {
		return manager.AuthorizeAny(ctx.Request.Context(), subject, permissions...)
	})
}

type middlewareCheck func(*gin.Context, string, []businessrbac.Permission) (bool, error)

func require(manager businessrbac.Manager, permissions []businessrbac.Permission, check middlewareCheck) gin.HandlerFunc {
	var constructionErr error
	if isNilManager(manager) {
		constructionErr = errors.New("rbac: middleware requires a non-nil Manager")
	}
	return func(ctx *gin.Context) {
		if ctx == nil {
			return
		}
		if constructionErr != nil {
			web.AbortError(ctx, constructionErr)
			return
		}
		principal, ok := web.CurrentPrincipal(ctx)
		if !ok {
			forbidden(ctx)
			return
		}
		if ctx.Request == nil {
			web.AbortError(ctx, errors.New("rbac: middleware request is unavailable"))
			return
		}

		allowed, err := check(ctx, principal.Subject, permissions)
		if err != nil {
			web.AbortError(ctx, fmt.Errorf("rbac: authorize HTTP principal %q: %w", principal.Subject, err))
			return
		}
		if !allowed {
			forbidden(ctx)
			return
		}
		ctx.Next()
	}
}

func forbidden(ctx *gin.Context) {
	web.AbortProblem(ctx, web.NewProblem(http.StatusForbidden, "forbidden"))
}

func isNilManager(manager businessrbac.Manager) bool {
	if manager == nil {
		return true
	}
	value := reflect.ValueOf(manager)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
