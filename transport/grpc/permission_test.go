package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/xbcio/xbc/plugin"
)

// permProbe reports what the authentication stage published for the call: the
// call outcome describeCall already covers, plus the method's permission.
type permProbe struct{}

func describePermCall(ctx context.Context) string {
	permission := "none"
	if value, declared := MethodPermission(ctx); declared {
		permission = value
	}
	return describeCall(ctx) + "/" + permission
}

func (permProbe) Echo(ctx context.Context, _ *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return wrapperspb.String(describePermCall(ctx)), nil
}

func (permProbe) Repeat(_ *wrapperspb.StringValue, stream grpc.ServerStream) error {
	return stream.SendMsg(wrapperspb.String(describePermCall(stream.Context())))
}

func permContributor(declare func(*MethodOptions)) plugin.Entry[ServiceRegistrar] {
	return plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "perm-probe"},
		Value: registrarFunc(func(r *Registrar) error {
			r.RegisterService(echoDesc(), permProbe{})
			if declare != nil {
				declare(r.Methods(echoServiceName))
			}
			return nil
		}),
	}
}

func TestMethodPermissionsFollowServiceDefaultsAndMethodOverrides(t *testing.T) {
	cases := map[string]struct {
		declare func(*MethodOptions)
		unary   string
		stream  string
	}{
		"no declaration": {
			declare: nil,
			unary:   "principal:alice/token/none",
			stream:  "principal:bob/token/none",
		},
		"service default applies to every method": {
			declare: func(m *MethodOptions) { m.Perm("orders:read") },
			unary:   "principal:alice/token/orders:read",
			stream:  "principal:bob/token/orders:read",
		},
		"method override applies to one method": {
			declare: func(m *MethodOptions) { m.Perm("orders:read").MethodPerm("Echo", "orders:write") },
			unary:   "principal:alice/token/orders:write",
			stream:  "principal:bob/token/orders:read",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newServer(DefaultConfig(), tokenOnly(permContributor(tc.declare)))
			_, conn := startServing(t, s)

			got, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice"))
			if err != nil || got != tc.unary {
				t.Fatalf("unary = %q, %v; want %q", got, err, tc.unary)
			}
			got, _, err = invokeRepeat(t, conn, outgoing("authorization", "Token good-bob"))
			if err != nil || got != tc.stream {
				t.Fatalf("stream = %q, %v; want %q", got, err, tc.stream)
			}
		})
	}
}

// TestPermittedCallsCarryTheMethodPermissionToo pins that the permission
// travels with exemption as well: an authorization interceptor reads the
// method's declaration, never the call's authentication outcome.
func TestPermittedCallsCarryTheMethodPermissionToo(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(permContributor(func(m *MethodOptions) {
		m.Auth(Public()).Perm("orders:read")
	})))
	_, conn := startServing(t, s)

	got, _, err := invokeEcho(conn, outgoing())
	if err != nil || got != "exempt/orders:read" {
		t.Fatalf("permitted unary = %q, %v; want exempt/orders:read", got, err)
	}
}

func TestMethodPermDeclarationsAreCheckedAgainstTheFrozenTable(t *testing.T) {
	other := plugin.Entry[ServiceRegistrar]{
		Identity: plugin.Identity{Plugin: "intruder"},
		Value: registrarFunc(func(r *Registrar) error {
			r.Methods(echoServiceName).Perm("orders:read")
			return nil
		}),
	}
	cases := map[string]struct {
		registrars []plugin.Entry[ServiceRegistrar]
		want       string
	}{
		"unknown service": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(nil), {
				Identity: plugin.Identity{Plugin: "ghost"},
				Value:    registrarFunc(func(r *Registrar) error { r.Methods("ghost.Service").Perm("x"); return nil }),
			}},
			want: `"ghost.Service", which no plugin registers`,
		},
		"another contributor's service": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(nil), other},
			want:       "only the registering plugin may declare it",
		},
		"unknown method": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.MethodPerm("Nope", "x") })},
			want:       "/xbc.test.Echo/Nope, which the service does not define",
		},
		"empty method": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.MethodPerm("", "x") })},
			want:       "with an empty method name",
		},
		"empty service permission": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Perm("  ") })},
			want:       "declared an empty permission",
		},
		"empty method permission": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.MethodPerm("Echo", "") })},
			want:       "declared an empty permission",
		},
		"declared twice": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) { m.Perm("a").Perm("b") })},
			want:       "more than once",
		},
		"method declared twice": {
			registrars: []plugin.Entry[ServiceRegistrar]{whoamiContributor(func(m *MethodOptions) {
				m.MethodPerm("Echo", "a").MethodPerm("Echo", "b")
			})},
			want: "more than once",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := tokenOnly(tc.registrars...)
			err := newServer(DefaultConfig(), c).Preflight(contextFromHost(newFakeHost()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Preflight error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestAuthAndPermCoexistForOneSlot pins that the two declaration axes are
// independent: a method may declare both without either counting as a
// duplicate of the other.
func TestAuthAndPermCoexistForOneSlot(t *testing.T) {
	s := newServer(DefaultConfig(), tokenOnly(permContributor(func(m *MethodOptions) {
		m.Auth(Public())
		m.MethodAuth("Echo", Accepts(schemeToken))
		m.Perm("orders:read")
		m.MethodPerm("Echo", "orders:write")
	})))
	_, conn := startServing(t, s)

	got, _, err := invokeEcho(conn, outgoing("authorization", "Token good-alice"))
	if err != nil || got != "principal:alice/token/orders:write" {
		t.Fatalf("unary = %q, %v; want the method's own auth and permission", got, err)
	}
	got, _, err = invokeRepeat(t, conn, outgoing())
	if err != nil || got != "exempt/orders:read" {
		t.Fatalf("stream = %q, %v; want the service defaults", got, err)
	}
}

func TestMethodPermissionFailsClosedWithoutTheStage(t *testing.T) {
	if permission, declared := MethodPermission(context.Background()); declared || permission != "" {
		t.Fatalf("a bare context reports %q, %v; want no permission", permission, declared)
	}
	// The stage publishes the fact with the empty string for an undeclared
	// method, and the reader must not report that as a declaration.
	if permission, declared := MethodPermission(withMethodPermission(context.Background(), "")); declared || permission != "" {
		t.Fatalf("an empty publication reports %q, %v; want no permission", permission, declared)
	}
}
