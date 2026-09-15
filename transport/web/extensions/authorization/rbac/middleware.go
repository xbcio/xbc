package rbac

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	businessrbac "github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/transport/web"
)

// RequireAll returns route/group middleware that requires every permission for
// the verified web.CurrentPrincipal subject.
func RequireAll(manager businessrbac.Manager, permissions ...businessrbac.Permission) web.Handler {
	return require(manager, append([]businessrbac.Permission(nil), permissions...), func(ctx context.Context, subject string, permissions []businessrbac.Permission) (bool, error) {
		return manager.AuthorizeAll(ctx, subject, permissions...)
	})
}

// RequireAny returns route/group middleware that requires at least one
// permission for the verified web.CurrentPrincipal subject.
func RequireAny(manager businessrbac.Manager, permissions ...businessrbac.Permission) web.Handler {
	return require(manager, append([]businessrbac.Permission(nil), permissions...), func(ctx context.Context, subject string, permissions []businessrbac.Permission) (bool, error) {
		return manager.AuthorizeAny(ctx, subject, permissions...)
	})
}

type middlewareCheck func(context.Context, string, []businessrbac.Permission) (bool, error)

func require(manager businessrbac.Manager, permissions []businessrbac.Permission, check middlewareCheck) web.Handler {
	var constructionErr error
	if isNilManager(manager) {
		constructionErr = errors.New("rbac: middleware requires a non-nil Manager")
	}
	return func(ctx context.Context, c *web.Ctx) error {
		if c == nil {
			return nil
		}
		if constructionErr != nil {
			web.AbortError(c, constructionErr)
			return nil
		}
		principal, ok := web.CurrentPrincipal(c)
		if !ok {
			forbidden(c)
			return nil
		}
		// Unreachable through web.Handle: Handle dereferences c.Request
		// before invoking the handler, so a nil request would already have
		// surfaced there. This guard remains for a direct caller that builds
		// *web.Ctx itself (see web.NewCtx).
		if c.Request() == nil {
			web.AbortError(c, errors.New("rbac: middleware request is unavailable"))
			return nil
		}

		allowed, err := check(ctx, principal.Subject, permissions)
		if err != nil {
			web.AbortError(c, fmt.Errorf("rbac: authorize HTTP principal %q: %w", principal.Subject, err))
			return nil
		}
		if !allowed {
			forbidden(c)
			return nil
		}
		c.Next()
		return nil
	}
}

func forbidden(c *web.Ctx) {
	web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
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
