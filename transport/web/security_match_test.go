package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseMatchRejectsUnsupportedSyntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "empty", raw: "   ", wantErr: "empty match"},
		{name: "relative path", raw: "api/**", wantErr: "must start with \"/\""},
		{name: "mid-segment wildcard", raw: "/api/**/detail", wantErr: "only a trailing \"/**\""},
		{name: "bare double star", raw: "**", wantErr: "must start with \"/\""},
		{name: "single star", raw: "/api/*", wantErr: "only a trailing \"/**\""},
		{name: "too many fields", raw: "GET /api /extra", wantErr: "expected \"[METHOD ]path\""},
		{name: "lowercase method is fine", raw: "get /api"},
		{name: "plain path", raw: "/api/v1/users"},
		{name: "trailing wildcard", raw: "/api/**"},
		{name: "root wildcard", raw: "/**"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseMatch(test.raw)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("parseMatch(%q) error = %v, want nil", test.raw, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseMatch(%q) error = nil, want substring %q", test.raw, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parseMatch(%q) error = %q, want substring %q", test.raw, err, test.wantErr)
			}
		})
	}
}

func TestMatcherMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		method string
		path   string
		want   bool
	}{
		{name: "exact path any method", raw: "/health", method: http.MethodGet, path: "/health", want: true},
		{name: "exact path wrong path", raw: "/health", method: http.MethodGet, path: "/healthz", want: false},
		{name: "method scoped hit", raw: "GET /health", method: http.MethodGet, path: "/health", want: true},
		{name: "method scoped miss", raw: "GET /health", method: http.MethodPost, path: "/health", want: false},
		{name: "method is case insensitive", raw: "get /health", method: http.MethodGet, path: "/health", want: true},
		{name: "wildcard matches deeper", raw: "/api/**", method: http.MethodGet, path: "/api/v1/users", want: true},
		{name: "wildcard matches prefix itself", raw: "/api/**", method: http.MethodGet, path: "/api", want: true},
		{name: "wildcard does not match sibling", raw: "/api/**", method: http.MethodGet, path: "/apix/v1", want: false},
		{name: "root wildcard matches everything", raw: "/**", method: http.MethodDelete, path: "/anything/at/all", want: true},
		{name: "root wildcard matches root", raw: "/**", method: http.MethodGet, path: "/", want: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m, err := parseMatch(test.raw)
			if err != nil {
				t.Fatalf("parseMatch(%q) error = %v", test.raw, err)
			}
			if got := m.matches(test.method, test.path); got != test.want {
				t.Fatalf("matcher(%q).matches(%s, %s) = %v, want %v", test.raw, test.method, test.path, got, test.want)
			}
		})
	}
}

func TestMatcherCoversDetectsShadowing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		broad  string
		narrow string
		want   bool
	}{
		{name: "wildcard covers nested exact", broad: "/api/**", narrow: "/api/v1/users", want: true},
		{name: "wildcard covers nested wildcard", broad: "/api/**", narrow: "/api/v1/**", want: true},
		{name: "any method covers specific method", broad: "/health", narrow: "GET /health", want: true},
		{name: "specific method does not cover any method", broad: "GET /health", narrow: "/health", want: false},
		{name: "exact does not cover wildcard", broad: "/api/v1/users", narrow: "/api/**", want: false},
		{name: "disjoint paths do not cover", broad: "/api/**", narrow: "/open/**", want: false},
		{name: "root wildcard covers all", broad: "/**", narrow: "POST /anything", want: true},
		{name: "different methods do not cover", broad: "GET /health", narrow: "POST /health", want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			broad, err := parseMatch(test.broad)
			if err != nil {
				t.Fatalf("parseMatch(%q) error = %v", test.broad, err)
			}
			narrow, err := parseMatch(test.narrow)
			if err != nil {
				t.Fatalf("parseMatch(%q) error = %v", test.narrow, err)
			}
			if got := broad.covers(narrow); got != test.want {
				t.Fatalf("matcher(%q).covers(%q) = %v, want %v", test.broad, test.narrow, got, test.want)
			}
		})
	}
}
