package federation

import (
	"context"
	"errors"
	"testing"
)

// stubIdentityResolver records its calls and returns a fixed outcome.
type stubIdentityResolver struct {
	resolution Resolution
	err        error
	calls      int
}

func (r *stubIdentityResolver) Resolve(context.Context, ExternalIdentity) (Resolution, error) {
	r.calls++
	return r.resolution, r.err
}

func TestResolvedTrimsSubject(t *testing.T) {
	resolution := Resolved("  user-1\t", nil)
	if !resolution.IsResolved() {
		t.Fatal("IsResolved() = false, want true")
	}
	if got := resolution.Subject(); got != "user-1" {
		t.Fatalf("Subject() = %q, want %q", got, "user-1")
	}
	if err := resolution.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestResolvedRejectsEmptySubject(t *testing.T) {
	for _, subject := range []string{"", " ", "\t\n", "   \r\n "} {
		resolution := Resolved(subject, nil)
		if err := resolution.Validate(); !errors.Is(err, ErrEmptySubject) {
			t.Fatalf("Resolved(%q).Validate() error = %v, want ErrEmptySubject", subject, err)
		}
	}
}

func TestZeroResolutionIsInvalid(t *testing.T) {
	var resolution Resolution
	if resolution.IsResolved() {
		t.Fatal("zero Resolution.IsResolved() = true, want false")
	}
	if err := resolution.Validate(); !errors.Is(err, ErrInvalidResolution) {
		t.Fatalf("zero Resolution.Validate() error = %v, want ErrInvalidResolution", err)
	}
	if got := resolution.Subject(); got != "" {
		t.Fatalf("zero Resolution.Subject() = %q, want empty", got)
	}
	if got := resolution.Attributes(); got != nil {
		t.Fatalf("zero Resolution.Attributes() = %v, want nil", got)
	}
	if reason, ok := resolution.Refusal(); ok || reason != "" {
		t.Fatalf("zero Resolution.Refusal() = (%q, %v), want (\"\", false)", reason, ok)
	}
}

func TestRefusedResolution(t *testing.T) {
	for _, reason := range []RefusalReason{RefusalUnbound, RefusalDisabled, RefusalBindingRequired, RefusalOther} {
		resolution := Refused(reason)
		if resolution.IsResolved() {
			t.Fatalf("Refused(%q).IsResolved() = true, want false", reason)
		}
		if err := resolution.Validate(); err != nil {
			t.Fatalf("Refused(%q).Validate() error = %v", reason, err)
		}
		got, ok := resolution.Refusal()
		if !ok || got != reason {
			t.Fatalf("Refused(%q).Refusal() = (%q, %v)", reason, got, ok)
		}
		if subject := resolution.Subject(); subject != "" {
			t.Fatalf("Refused(%q).Subject() = %q, want empty", reason, subject)
		}
		if attributes := resolution.Attributes(); attributes != nil {
			t.Fatalf("Refused(%q).Attributes() = %v, want nil", reason, attributes)
		}
	}

	if got, _ := Refused("").Refusal(); got != RefusalOther {
		t.Fatalf("Refused(\"\").Refusal() = %q, want %q", got, RefusalOther)
	}
}

func TestResolutionCopiesAttributes(t *testing.T) {
	nested := map[string]any{"team": "core"}
	source := map[string]any{
		"role":   "admin",
		"nested": nested,
		"groups": []any{"engineering", "oncall"},
		"tags":   []string{"a", "b"},
	}
	resolution := Resolved("user-1", source)

	// Mutating the caller's map after Resolved must not reach the resolution.
	source["role"] = "mutated"
	nested["team"] = "mutated"
	source["groups"].([]any)[0] = "mutated"
	source["tags"].([]string)[0] = "mutated"

	got := resolution.Attributes()
	if got["role"] != "admin" {
		t.Fatalf("role = %v, want admin", got["role"])
	}
	if got["nested"].(map[string]any)["team"] != "core" {
		t.Fatal("Resolved retained the caller's nested map")
	}
	if got["groups"].([]any)[0] != "engineering" {
		t.Fatal("Resolved retained the caller's slice")
	}
	if got["tags"].([]string)[0] != "a" {
		t.Fatal("Resolved retained the caller's string slice")
	}

	// Mutating a returned copy must not reach the resolution either.
	got["role"] = "mutated-again"
	got["nested"].(map[string]any)["team"] = "mutated-again"
	got["groups"].([]any)[0] = "mutated-again"
	got["tags"].([]string)[0] = "mutated-again"

	again := resolution.Attributes()
	if again["role"] != "admin" || again["nested"].(map[string]any)["team"] != "core" ||
		again["groups"].([]any)[0] != "engineering" || again["tags"].([]string)[0] != "a" {
		t.Fatalf("Attributes() leaked a mutable reference: %v", again)
	}
}

func TestResolutionCopiesCommonMapShapes(t *testing.T) {
	labels := map[string]string{"cn": "alice"}
	aliases := map[string][]string{"mail": {"a@example.test", "b@example.test"}}
	resolution := Resolved("user-1", map[string]any{"labels": labels, "aliases": aliases})

	// Mutating the caller's maps after Resolved must not reach the resolution.
	labels["cn"] = "mutated"
	aliases["mail"][0] = "mutated"

	got := resolution.Attributes()
	if got["labels"].(map[string]string)["cn"] != "alice" {
		t.Fatal("Resolved retained the caller's map[string]string")
	}
	if got["aliases"].(map[string][]string)["mail"][0] != "a@example.test" {
		t.Fatal("Resolved retained the caller's map[string][]string slice")
	}

	// Mutating a returned copy must not reach the resolution either.
	got["labels"].(map[string]string)["cn"] = "mutated-again"
	got["aliases"].(map[string][]string)["mail"][0] = "mutated-again"

	again := resolution.Attributes()
	if again["labels"].(map[string]string)["cn"] != "alice" {
		t.Fatal("Attributes() leaked a mutable map[string]string")
	}
	if again["aliases"].(map[string][]string)["mail"][0] != "a@example.test" {
		t.Fatal("Attributes() leaked a mutable map[string][]string slice")
	}
}

func TestResolvedRejectsNonJSONAttributes(t *testing.T) {
	resolution := Resolved("user-1", map[string]any{"callback": make(chan int)})
	if err := resolution.Validate(); !errors.Is(err, ErrInvalidAttribute) {
		t.Fatalf("Validate() error = %v, want ErrInvalidAttribute", err)
	}
}

func TestResolveIdentityRejectsNilResolver(t *testing.T) {
	if _, err := ResolveIdentity(context.Background(), nil, ExternalIdentity{}); !errors.Is(err, ErrNilResolver) {
		t.Fatalf("ResolveIdentity(nil) error = %v, want ErrNilResolver", err)
	}

	var typedNil *stubIdentityResolver
	if _, err := ResolveIdentity(context.Background(), typedNil, ExternalIdentity{}); !errors.Is(err, ErrNilResolver) {
		t.Fatalf("ResolveIdentity(typed nil) error = %v, want ErrNilResolver", err)
	}
}

func TestResolveIdentityRejectsNilContext(t *testing.T) {
	resolver := &stubIdentityResolver{resolution: Resolved("user-1", nil)}
	//lint:ignore SA1012 the contract explicitly rejects a nil context.
	_, err := ResolveIdentity(nil, resolver, ExternalIdentity{})
	if !errors.Is(err, ErrNilContext) {
		t.Fatalf("ResolveIdentity(nil context) error = %v, want ErrNilContext", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", resolver.calls)
	}
}

func TestResolveIdentityPropagatesResolverError(t *testing.T) {
	cause := errors.New("storage unavailable")
	resolver := &stubIdentityResolver{resolution: Resolved("user-1", nil), err: cause}
	resolution, err := ResolveIdentity(context.Background(), resolver, ExternalIdentity{})
	if !errors.Is(err, cause) {
		t.Fatalf("ResolveIdentity() error = %v, want to wrap %v", err, cause)
	}
	if resolution.IsResolved() {
		t.Fatal("ResolveIdentity() returned a resolved resolution alongside an error")
	}
	if err.Error() != "federation: identity resolution failed: storage unavailable" {
		t.Fatalf("error text = %q", err.Error())
	}
}

func TestResolveIdentityRejectsInvalidResolution(t *testing.T) {
	for name, resolution := range map[string]Resolution{
		"empty subject": Resolved("", nil),
		"zero value":    {},
		"non-JSON":      Resolved("user-1", map[string]any{"bad": make(chan int)}),
	} {
		resolver := &stubIdentityResolver{resolution: resolution}
		got, err := ResolveIdentity(context.Background(), resolver, ExternalIdentity{})
		if err == nil {
			t.Fatalf("%s: ResolveIdentity() error = nil, want an operational failure", name)
		}
		if got.IsResolved() {
			t.Fatalf("%s: ResolveIdentity() returned a resolved resolution on failure", name)
		}
	}
}

func TestResolveIdentityReturnsOutcomes(t *testing.T) {
	resolved := &stubIdentityResolver{resolution: Resolved("user-1", map[string]any{"role": "admin"})}
	got, err := ResolveIdentity(context.Background(), resolved, ExternalIdentity{Provider: "corp"})
	if err != nil {
		t.Fatalf("ResolveIdentity() error = %v", err)
	}
	if !got.IsResolved() || got.Subject() != "user-1" || got.Attributes()["role"] != "admin" {
		t.Fatalf("ResolveIdentity() = %#v", got)
	}
	if resolved.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolved.calls)
	}

	refused := &stubIdentityResolver{resolution: Refused(RefusalUnbound)}
	got, err = ResolveIdentity(context.Background(), refused, ExternalIdentity{})
	if err != nil {
		t.Fatalf("ResolveIdentity(refusal) error = %v", err)
	}
	if reason, ok := got.Refusal(); !ok || reason != RefusalUnbound {
		t.Fatalf("ResolveIdentity(refusal) = (%q, %v), want RefusalUnbound", reason, ok)
	}
}

func TestRefusalReasonValuesAreStable(t *testing.T) {
	cases := []struct {
		reason RefusalReason
		want   string
	}{
		{RefusalUnbound, "unbound"},
		{RefusalDisabled, "disabled"},
		{RefusalBindingRequired, "binding_required"},
		{RefusalOther, "other"},
	}
	for _, testCase := range cases {
		if string(testCase.reason) != testCase.want {
			t.Fatalf("refusal reason = %q, want %q", string(testCase.reason), testCase.want)
		}
	}
}
