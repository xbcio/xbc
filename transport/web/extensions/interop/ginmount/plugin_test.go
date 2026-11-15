package ginmount

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xbcio/xbc/plugin"
)

// stubRegistrar is a Registrar with a fixed path and no routes, so the build
// validation can be exercised without an application.
type stubRegistrar struct{ path string }

func (s stubRegistrar) MountPath() string { return s.path }

func (stubRegistrar) RegisterGin(*gin.Engine) {}

func TestNewPluginValidatesMountPath(t *testing.T) {
	stubEntry := func(path string) []plugin.Entry[Registrar] {
		return []plugin.Entry[Registrar]{{
			Identity: plugin.Identity{Plugin: "stub"},
			Value:    stubRegistrar{path: path},
		}}
	}

	t.Run("rejected", func(t *testing.T) {
		for _, path := range []string{"", "/", "/api/", "/api/:version", "/api/*rest", "api/v1"} {
			t.Run(path, func(t *testing.T) {
				_, err := newPlugin(plugin.BuildContext{}, stubEntry(path))
				if err == nil {
					t.Fatalf("newPlugin accepted MountPath %q, which cannot be mounted", path)
				}
				for _, want := range []string{strconv.Quote(path), "stub"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			})
		}
	})

	t.Run("accepted", func(t *testing.T) {
		for _, path := range []string{"/api/v1", "/v2", "/a/b"} {
			t.Run(path, func(t *testing.T) {
				p, err := newPlugin(plugin.BuildContext{}, stubEntry(path))
				if err != nil {
					t.Fatalf("newPlugin rejected MountPath %q: %v", path, err)
				}
				if len(p.registrations) != 1 || p.registrations[0].path != path {
					t.Fatalf("registrations = %+v, want one entry for %q", p.registrations, path)
				}
			})
		}
	})
}

func TestNewPluginRejectsNilRegistrar(t *testing.T) {
	_, err := newPlugin(plugin.BuildContext{}, []plugin.Entry[Registrar]{{
		Identity: plugin.Identity{Plugin: "stub"},
		Value:    nil,
	}})
	if err == nil {
		t.Fatal("newPlugin accepted a nil Registrar")
	}
	if !strings.Contains(err.Error(), "stub") {
		t.Errorf("error %q does not name the exporting plugin", err)
	}
}

// TestNewPluginKeepsSharedPrefixes pins the contract that one prefix may be
// declared by several Registrars: the engine is shared at registration time,
// so the build must keep every declaration rather than the first one.
func TestNewPluginKeepsSharedPrefixes(t *testing.T) {
	entries := []plugin.Entry[Registrar]{
		{Identity: plugin.Identity{Plugin: "first"}, Value: stubRegistrar{path: "/api/v1"}},
		{Identity: plugin.Identity{Plugin: "second"}, Value: stubRegistrar{path: "/api/v1"}},
	}
	p, err := newPlugin(plugin.BuildContext{}, entries)
	if err != nil {
		t.Fatalf("newPlugin rejected a shared MountPath: %v", err)
	}
	if len(p.registrations) != 2 {
		t.Fatalf("registrations = %+v, want both declarations", p.registrations)
	}
	for i, want := range []string{"/api/v1", "/api/v1"} {
		if p.registrations[i].path != want {
			t.Errorf("registration %d path = %q, want %q", i, p.registrations[i].path, want)
		}
	}
}
