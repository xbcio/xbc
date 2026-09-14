package rbac

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	businessrbac "github.com/xbcio/xbc/extensions/authorization/rbac"
	"github.com/xbcio/xbc/transport/web"
)

type middlewareManager struct {
	allFunc func(context.Context, string, ...businessrbac.Permission) (bool, error)
	anyFunc func(context.Context, string, ...businessrbac.Permission) (bool, error)
}

func (*middlewareManager) IsAdmin(context.Context, string) (bool, error) { return false, nil }
func (*middlewareManager) Authorize(context.Context, string, businessrbac.Permission) (bool, error) {
	return false, nil
}
func (manager *middlewareManager) AuthorizeAll(ctx context.Context, subject string, permissions ...businessrbac.Permission) (bool, error) {
	return manager.allFunc(ctx, subject, permissions...)
}
func (manager *middlewareManager) AuthorizeAny(ctx context.Context, subject string, permissions ...businessrbac.Permission) (bool, error) {
	return manager.anyFunc(ctx, subject, permissions...)
}
func (*middlewareManager) HasRole(context.Context, string, string) (bool, error) { return false, nil }
func (*middlewareManager) Roles(context.Context, string) ([]string, error)       { return nil, nil }
func (*middlewareManager) RolePermissions(context.Context, string) ([]businessrbac.Permission, error) {
	return nil, nil
}
func (*middlewareManager) ReplaceSubjectRoles(context.Context, string, []string) (bool, error) {
	return false, nil
}
func (*middlewareManager) ReplaceRolePermissions(context.Context, string, []businessrbac.Permission) (bool, error) {
	return false, nil
}
func (*middlewareManager) DeleteSubject(context.Context, string) (bool, error) { return false, nil }
func (*middlewareManager) DeleteRole(context.Context, string) (bool, error)    { return false, nil }

type middlewareContextKey struct{}

func TestRequireAllUsesOnlyCurrentPrincipalAndAllowsAuthorizedRequest(t *testing.T) {
	permissions := []businessrbac.Permission{{Object: "reports", Action: "read"}}
	manager := &middlewareManager{}
	manager.allFunc = func(ctx context.Context, subject string, got ...businessrbac.Permission) (bool, error) {
		if ctx.Value(middlewareContextKey{}) != "request" {
			t.Fatal("request context was not forwarded")
		}
		if subject != "alice" || !reflect.DeepEqual(got, permissions) {
			t.Fatalf("authorization input = %q, %#v", subject, got)
		}
		return true, nil
	}
	manager.anyFunc = func(context.Context, string, ...businessrbac.Permission) (bool, error) {
		t.Fatal("RequireAll called AuthorizeAny")
		return false, nil
	}

	requestContext := context.WithValue(context.Background(), middlewareContextKey{}, "request")
	response, reached := serveRBAC(t, RequireAll(manager, permissions...), &web.Principal{Subject: "alice", AuthMethod: "test"}, requestContext, "")
	if response.Code != http.StatusNoContent || !reached {
		t.Fatalf("response = %d body=%q reached=%v", response.Code, response.Body.String(), reached)
	}
}

func TestRequireAnyDelegatesAnyAndDefensivelyCopiesPermissions(t *testing.T) {
	permissions := []businessrbac.Permission{{Object: "reports", Action: "read"}}
	manager := &middlewareManager{}
	manager.allFunc = func(context.Context, string, ...businessrbac.Permission) (bool, error) {
		t.Fatal("RequireAny called AuthorizeAll")
		return false, nil
	}
	manager.anyFunc = func(_ context.Context, subject string, got ...businessrbac.Permission) (bool, error) {
		want := []businessrbac.Permission{{Object: "reports", Action: "read"}}
		if subject != "alice" || !reflect.DeepEqual(got, want) {
			t.Fatalf("authorization input = %q, %#v", subject, got)
		}
		return true, nil
	}
	handler := RequireAny(manager, permissions...)
	permissions[0] = businessrbac.Permission{Object: "mutated", Action: "delete"}

	response, reached := serveRBAC(t, handler, &web.Principal{Subject: "alice"}, context.Background(), "")
	if response.Code != http.StatusNoContent || !reached {
		t.Fatalf("response = %d body=%q reached=%v", response.Code, response.Body.String(), reached)
	}
}

func TestRequireAllRejectsAnonymousAndIgnoresAuthorizationHeader(t *testing.T) {
	calls := 0
	manager := &middlewareManager{
		allFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) {
			calls++
			return true, nil
		},
		anyFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) { return true, nil },
	}
	response, reached := serveRBAC(t, RequireAll(manager, businessrbac.Permission{Object: "reports"}), nil, context.Background(), "Bearer unverified")
	assertProblem(t, response, http.StatusForbidden, "forbidden")
	if reached || calls != 0 {
		t.Fatalf("anonymous request reached=%v manager calls=%d", reached, calls)
	}
}

func TestRequireAnyReturnsForbiddenOnDenial(t *testing.T) {
	manager := &middlewareManager{
		allFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) { return false, nil },
		anyFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) { return false, nil },
	}
	response, reached := serveRBAC(t, RequireAny(manager, businessrbac.Permission{Object: "reports"}), &web.Principal{Subject: "bob"}, context.Background(), "")
	assertProblem(t, response, http.StatusForbidden, "forbidden")
	if reached {
		t.Fatal("denied request reached endpoint")
	}
}

func TestRequireAllSendsManagerErrorsThroughSafeBoundary(t *testing.T) {
	managerErr := errors.New("database password=secret")
	manager := &middlewareManager{
		allFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) { return false, managerErr },
		anyFunc: func(context.Context, string, ...businessrbac.Permission) (bool, error) { return false, managerErr },
	}
	response, reached := serveRBAC(t, RequireAll(manager, businessrbac.Permission{Object: "reports"}), &web.Principal{Subject: "alice"}, context.Background(), "")
	assertProblem(t, response, http.StatusInternalServerError, "internal_server_error")
	if reached || strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("unsafe error response body=%q reached=%v", response.Body.String(), reached)
	}
}

func TestRequireAllTreatsNilAndTypedNilManagersAsInternalErrors(t *testing.T) {
	var typedNil *middlewareManager
	for name, manager := range map[string]businessrbac.Manager{"nil": nil, "typed nil": typedNil} {
		t.Run(name, func(t *testing.T) {
			response, reached := serveRBAC(t, RequireAll(manager, businessrbac.Permission{Object: "reports"}), &web.Principal{Subject: "alice"}, context.Background(), "")
			assertProblem(t, response, http.StatusInternalServerError, "internal_server_error")
			if reached {
				t.Fatal("invalid Manager request reached endpoint")
			}
		})
	}
}

func serveRBAC(t *testing.T, middleware gin.HandlerFunc, principal *web.Principal, requestContext context.Context, authorization string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(web.Handle(web.OnError()))
	if principal != nil {
		engine.Use(func(ctx *gin.Context) {
			if !web.SetPrincipal(ctx, *principal) {
				t.Fatal("failed to install test principal")
			}
			ctx.Next()
		})
	}
	engine.Use(middleware)
	reached := false
	engine.GET("/reports", func(ctx *gin.Context) {
		reached = true
		ctx.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/reports", nil).WithContext(requestContext)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response, reached
}

func assertProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, status, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/problem+json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	var problem web.ProblemDetail
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Status != status || problem.Properties["code"] != code || problem.Instance != "/reports" {
		t.Fatalf("problem = %#v", problem)
	}
}
