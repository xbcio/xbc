package authentication

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	schemeAPIKey Scheme = "apikey"
	schemeJWT    Scheme = "jwt"
	schemeMTLS   Scheme = "mtls"
)

type stubAuthenticator struct {
	scheme Scheme
	calls  int
	input  any
	result Result
	err    error
}

func (a *stubAuthenticator) Scheme() Scheme { return a.scheme }

func (a *stubAuthenticator) Authenticate(_ context.Context, credential Credential) (Result, error) {
	a.calls++
	a.input = credential.Value()
	return a.result, a.err
}

func newTestManager(t *testing.T, defaults []Scheme, authenticators ...Authenticator) *Manager {
	t.Helper()
	manager, err := NewManager(ManagerOptions{
		Authenticators: authenticators,
		DefaultSchemes: defaults,
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func TestManagerUsesAuthenticationDomainOrderForSelectionAndChallenges(t *testing.T) {
	t.Parallel()

	apiKey := &stubAuthenticator{scheme: schemeAPIKey, result: Accepted("api-user")}
	jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted("jwt-user")}
	mtls := &stubAuthenticator{scheme: schemeMTLS, result: Accepted("mtls-user")}
	manager := newTestManager(
		t,
		[]Scheme{schemeMTLS, schemeAPIKey},
		apiKey,
		jwt,
		mtls,
	)

	if got, want := manager.Schemes(), []Scheme{schemeAPIKey, schemeJWT, schemeMTLS}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Schemes() = %v, want %v", got, want)
	}
	if got, want := manager.DefaultSchemes(), []Scheme{schemeAPIKey, schemeMTLS}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultSchemes() = %v, want %v", got, want)
	}

	var called []Scheme
	result, err := manager.Authenticate(
		context.Background(),
		SelectSchemes(schemeMTLS, schemeAPIKey),
		CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
			called = append(called, scheme)
			switch scheme {
			case schemeAPIKey:
				return AbsentWithChallenge("ApiKey"), nil
			case schemeMTLS:
				return AbsentWithChallenge("Mutual"), nil
			default:
				t.Fatalf("unexpected selected scheme %q", scheme)
				return CredentialResult{}, nil
			}
		}),
	)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got, want := called, []Scheme{schemeAPIKey, schemeMTLS}; !reflect.DeepEqual(got, want) {
		t.Fatalf("credential order = %v, want %v", got, want)
	}
	assertRejection(t, result, RejectionUnauthenticated, ReasonUnauthenticated)
	if got, want := result.Challenges(), []Challenge{"ApiKey", "Mutual"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Challenges() = %v, want %v", got, want)
	}
}

func TestNewManagerRejectsDuplicateSchemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options ManagerOptions
		want    string
	}{
		{
			name: "authenticators",
			options: ManagerOptions{
				Authenticators: []Authenticator{
					&stubAuthenticator{scheme: schemeJWT},
					&stubAuthenticator{scheme: schemeJWT},
				},
				DefaultSchemes: []Scheme{schemeJWT},
			},
			want: `"jwt" in authenticators at ordered positions 0 and 1`,
		},
		{
			name: "defaults",
			options: ManagerOptions{
				Authenticators: []Authenticator{&stubAuthenticator{scheme: schemeJWT}},
				DefaultSchemes: []Scheme{schemeJWT, schemeJWT},
			},
			want: `"jwt" in defaults at positions 0 and 1`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewManager(test.options)
			if !errors.Is(err, ErrDuplicateScheme) {
				t.Fatalf("NewManager() error = %v, want ErrDuplicateScheme", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewManager() error = %q, want substring %q", err, test.want)
			}
		})
	}
}

func TestSelectionRejectsDuplicateAndUnknownSchemesBeforeExtraction(t *testing.T) {
	t.Parallel()

	manager := newTestManager(
		t,
		[]Scheme{schemeJWT},
		&stubAuthenticator{scheme: schemeAPIKey},
		&stubAuthenticator{scheme: schemeJWT},
	)
	sourceCalls := 0
	source := CredentialSourceFunc(func(context.Context, Scheme) (CredentialResult, error) {
		sourceCalls++
		return Absent(), nil
	})

	_, err := manager.Authenticate(
		context.Background(),
		SelectSchemes(schemeJWT, schemeJWT),
		source,
	)
	if !errors.Is(err, ErrDuplicateScheme) {
		t.Fatalf("duplicate selection error = %v, want ErrDuplicateScheme", err)
	}
	if !strings.Contains(err.Error(), "positions 0 and 1") {
		t.Fatalf("duplicate selection diagnostic = %q, want both positions", err)
	}

	_, err = manager.Authenticate(
		context.Background(),
		SelectSchemes(Scheme("unknown")),
		source,
	)
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("unknown selection error = %v, want ErrUnknownScheme", err)
	}
	if !strings.Contains(err.Error(), `"unknown"`) ||
		!strings.Contains(err.Error(), `"apikey", "jwt"`) {
		t.Fatalf("unknown selection diagnostic = %q, want unknown and ordered registered schemes", err)
	}
	if sourceCalls != 0 {
		t.Fatalf("credential source called %d times for invalid selections, want 0", sourceCalls)
	}
}

func TestNewManagerRejectsUnknownDefaultScheme(t *testing.T) {
	t.Parallel()

	_, err := NewManager(ManagerOptions{
		Authenticators: []Authenticator{&stubAuthenticator{scheme: schemeJWT}},
		DefaultSchemes: []Scheme{schemeAPIKey},
	})
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("NewManager() error = %v, want ErrUnknownScheme", err)
	}
	if !strings.Contains(err.Error(), `"apikey" in defaults`) ||
		!strings.Contains(err.Error(), `registered schemes: "jwt"`) {
		t.Fatalf("NewManager() diagnostic = %q, want unknown and registered schemes", err)
	}
}

func TestManagerAuthenticatesSolePresentedCredentialAfterCollectingAllSchemes(t *testing.T) {
	t.Parallel()

	apiKey := &stubAuthenticator{scheme: schemeAPIKey, result: Accepted("unused")}
	jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted(struct{ Subject string }{"alice"})}
	manager := newTestManager(t, []Scheme{schemeAPIKey, schemeJWT}, apiKey, jwt)

	var extracted []Scheme
	result, err := manager.Authenticate(
		context.Background(),
		DefaultSelection(),
		CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
			extracted = append(extracted, scheme)
			if scheme == schemeJWT {
				return Presented("signed-token"), nil
			}
			return AbsentWithChallenge("ApiKey"), nil
		}),
	)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got, want := extracted, []Scheme{schemeAPIKey, schemeJWT}; !reflect.DeepEqual(got, want) {
		t.Fatalf("extracted schemes = %v, want %v", got, want)
	}
	if !result.Authenticated() || result.Status() != ResultStatusAuthenticated {
		t.Fatalf("result status = %s, want authenticated", result.Status())
	}
	if result.Rejected() {
		t.Fatal("authenticated result also reported rejected")
	}
	if got, ok := result.Scheme(); !ok || got != schemeJWT {
		t.Fatalf("Scheme() = %q, %v, want jwt, true", got, ok)
	}
	principal, ok := result.Principal()
	if !ok || !reflect.DeepEqual(principal, struct{ Subject string }{"alice"}) {
		t.Fatalf("Principal() = %#v, %v", principal, ok)
	}
	if jwt.calls != 1 || jwt.input != "signed-token" {
		t.Fatalf("jwt calls/input = %d/%#v, want 1/signed-token", jwt.calls, jwt.input)
	}
	if apiKey.calls != 0 {
		t.Fatalf("api key authenticator calls = %d, want 0", apiKey.calls)
	}
}

func TestRejectedCredentialIsTerminalAndDoesNotFallThrough(t *testing.T) {
	t.Parallel()

	apiKey := &stubAuthenticator{
		scheme: schemeAPIKey,
		result: RejectedWithChallenge("key not accepted", "ApiKey"),
	}
	jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted("jwt-user")}
	manager := newTestManager(t, []Scheme{schemeAPIKey, schemeJWT}, apiKey, jwt)

	result, err := manager.Authenticate(
		context.Background(),
		DefaultSelection(),
		CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
			if scheme == schemeAPIKey {
				return Presented("bad-key"), nil
			}
			return Absent(), nil
		}),
	)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	assertRejection(t, result, RejectionInvalidCredential, "key not accepted")
	if got, ok := result.Scheme(); !ok || got != schemeAPIKey {
		t.Fatalf("Scheme() = %q, %v, want apikey, true", got, ok)
	}
	if got, want := result.Challenges(), []Challenge{"ApiKey"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Challenges() = %v, want %v", got, want)
	}
	if apiKey.calls != 1 || jwt.calls != 0 {
		t.Fatalf("authenticator calls = apikey:%d jwt:%d, want 1/0", apiKey.calls, jwt.calls)
	}
}

func TestAuthenticatorOperationalErrorIsTerminalAndDiagnosticIsSafe(t *testing.T) {
	t.Parallel()

	cause := errors.New("database failed while checking secret-token-value")
	apiKey := &stubAuthenticator{scheme: schemeAPIKey, err: cause}
	jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted("jwt-user")}
	manager := newTestManager(t, []Scheme{schemeAPIKey, schemeJWT}, apiKey, jwt)

	result, err := manager.Authenticate(
		context.Background(),
		DefaultSelection(),
		CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
			if scheme == schemeAPIKey {
				return Presented("secret-token-value"), nil
			}
			return Absent(), nil
		}),
	)
	if err == nil {
		t.Fatal("Authenticate() error = nil, want operational error")
	}
	if result.Status() != 0 {
		t.Fatalf("result status = %v, want zero result on operational error", result.Status())
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Authenticate() error = %v, want wrapped cause", err)
	}
	var operational *OperationalError
	if !errors.As(err, &operational) {
		t.Fatalf("Authenticate() error type = %T, want *OperationalError", err)
	}
	if operational.Operation() != OperationAuthentication || operational.Scheme() != schemeAPIKey {
		t.Fatalf("operational metadata = %s/%q, want authentication/apikey", operational.Operation(), operational.Scheme())
	}
	if strings.Contains(err.Error(), "secret-token-value") || strings.Contains(err.Error(), "database failed") {
		t.Fatalf("safe diagnostic leaked cause or credential: %q", err)
	}
	if got, want := err.Error(), `authentication: authentication failed for scheme "apikey"`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if apiKey.calls != 1 || jwt.calls != 0 {
		t.Fatalf("authenticator calls = apikey:%d jwt:%d, want 1/0", apiKey.calls, jwt.calls)
	}
}

func TestDefaultFirstApplicableCombinations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		results           map[Scheme]CredentialResult
		wantKind          RejectionKind
		wantReason        SafeReason
		wantScheme        Scheme
		wantChallenges    []Challenge
		wantAPIKeyCalls   int
		wantJWTCalls      int
		wantAuthenticated bool
	}{
		{
			name: "all absent returns ordered deduplicated challenges",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: AbsentWithChallenge("Shared"),
				schemeJWT:    AbsentWithChallenge("Shared"),
				schemeMTLS:   AbsentWithChallenge("Mutual"),
			},
			wantKind:       RejectionUnauthenticated,
			wantReason:     ReasonUnauthenticated,
			wantChallenges: []Challenge{"Shared", "Mutual"},
		},
		{
			name: "sole malformed is terminal",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: Absent(),
				schemeJWT:    MalformedWithChallenge("bad token syntax", "Bearer"),
				schemeMTLS:   Absent(),
			},
			wantKind:       RejectionMalformedCredential,
			wantReason:     "bad token syntax",
			wantScheme:     schemeJWT,
			wantChallenges: []Challenge{"Bearer"},
		},
		{
			name: "presented plus malformed is ambiguous",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: Presented("key"),
				schemeJWT:    Malformed("bad token syntax"),
				schemeMTLS:   Absent(),
			},
			wantKind:   RejectionAmbiguousCredentials,
			wantReason: ReasonAmbiguousCredentials,
		},
		{
			name: "two presented credentials are ambiguous",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: Presented("key"),
				schemeJWT:    Presented("token"),
				schemeMTLS:   Absent(),
			},
			wantKind:   RejectionAmbiguousCredentials,
			wantReason: ReasonAmbiguousCredentials,
		},
		{
			name: "two malformed credentials are ambiguous",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: Malformed("bad key syntax"),
				schemeJWT:    Malformed("bad token syntax"),
				schemeMTLS:   Absent(),
			},
			wantKind:   RejectionAmbiguousCredentials,
			wantReason: ReasonAmbiguousCredentials,
		},
		{
			name: "sole accepted credential authenticates",
			results: map[Scheme]CredentialResult{
				schemeAPIKey: Absent(),
				schemeJWT:    Presented("valid-token"),
				schemeMTLS:   Absent(),
			},
			wantScheme:        schemeJWT,
			wantJWTCalls:      1,
			wantAuthenticated: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			apiKey := &stubAuthenticator{scheme: schemeAPIKey, result: Accepted("api-user")}
			jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted("jwt-user")}
			mtls := &stubAuthenticator{scheme: schemeMTLS, result: Accepted("mtls-user")}
			manager := newTestManager(
				t,
				[]Scheme{schemeJWT, schemeAPIKey, schemeMTLS},
				apiKey,
				jwt,
				mtls,
			)

			result, err := manager.Authenticate(
				context.Background(),
				DefaultSelection(),
				CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
					return test.results[scheme], nil
				}),
			)
			if err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}
			if test.wantAuthenticated {
				if !result.Authenticated() {
					t.Fatalf("result status = %s, want authenticated", result.Status())
				}
				if got, ok := result.Scheme(); !ok || got != test.wantScheme {
					t.Fatalf("Scheme() = %q, %v, want %q, true", got, ok, test.wantScheme)
				}
			} else {
				assertRejection(t, result, test.wantKind, test.wantReason)
				gotScheme, hasScheme := result.Scheme()
				if test.wantScheme == "" && hasScheme {
					t.Fatalf("Scheme() = %q, true, want no single scheme", gotScheme)
				}
				if test.wantScheme != "" && (!hasScheme || gotScheme != test.wantScheme) {
					t.Fatalf("Scheme() = %q, %v, want %q, true", gotScheme, hasScheme, test.wantScheme)
				}
			}
			if got := result.Challenges(); !reflect.DeepEqual(got, test.wantChallenges) {
				t.Fatalf("Challenges() = %v, want %v", got, test.wantChallenges)
			}
			if apiKey.calls != test.wantAPIKeyCalls || jwt.calls != test.wantJWTCalls || mtls.calls != 0 {
				t.Fatalf(
					"authenticator calls = apikey:%d jwt:%d mtls:%d, want %d/%d/0",
					apiKey.calls,
					jwt.calls,
					mtls.calls,
					test.wantAPIKeyCalls,
					test.wantJWTCalls,
				)
			}
		})
	}
}

func TestCredentialSourceFailureCollectsEverySchemeButInvokesNoAuthenticator(t *testing.T) {
	t.Parallel()

	cause := errors.New("extractor included sensitive raw credential")
	apiKey := &stubAuthenticator{scheme: schemeAPIKey, result: Accepted("api-user")}
	jwt := &stubAuthenticator{scheme: schemeJWT, result: Accepted("jwt-user")}
	manager := newTestManager(t, []Scheme{schemeAPIKey, schemeJWT}, apiKey, jwt)

	var called []Scheme
	_, err := manager.Authenticate(
		context.Background(),
		DefaultSelection(),
		CredentialSourceFunc(func(_ context.Context, scheme Scheme) (CredentialResult, error) {
			called = append(called, scheme)
			if scheme == schemeAPIKey {
				return CredentialResult{}, cause
			}
			return Presented("valid-token"), nil
		}),
	)
	if !errors.Is(err, cause) {
		t.Fatalf("Authenticate() error = %v, want wrapped source cause", err)
	}
	if got, want := called, []Scheme{schemeAPIKey, schemeJWT}; !reflect.DeepEqual(got, want) {
		t.Fatalf("credential source calls = %v, want %v", got, want)
	}
	if strings.Contains(err.Error(), "sensitive raw credential") {
		t.Fatalf("safe diagnostic leaked source cause: %q", err)
	}
	if apiKey.calls != 0 || jwt.calls != 0 {
		t.Fatalf("authenticator calls = apikey:%d jwt:%d, want 0/0", apiKey.calls, jwt.calls)
	}
}

func TestInvalidClosedResultsAreOperationalFailures(t *testing.T) {
	t.Parallel()

	t.Run("zero credential result", func(t *testing.T) {
		t.Parallel()
		authenticator := &stubAuthenticator{scheme: schemeJWT, result: Accepted("user")}
		manager := newTestManager(t, []Scheme{schemeJWT}, authenticator)
		_, err := manager.Authenticate(
			context.Background(),
			DefaultSelection(),
			CredentialSourceFunc(func(context.Context, Scheme) (CredentialResult, error) {
				return CredentialResult{}, nil
			}),
		)
		if !errors.Is(err, ErrInvalidCredentialResult) {
			t.Fatalf("Authenticate() error = %v, want ErrInvalidCredentialResult", err)
		}
		if authenticator.calls != 0 {
			t.Fatalf("authenticator calls = %d, want 0", authenticator.calls)
		}
	})

	t.Run("typed nil principal", func(t *testing.T) {
		t.Parallel()
		var principal *struct{ Subject string }
		authenticator := &stubAuthenticator{scheme: schemeJWT, result: Accepted(principal)}
		manager := newTestManager(t, []Scheme{schemeJWT}, authenticator)
		_, err := manager.Authenticate(
			context.Background(),
			DefaultSelection(),
			CredentialSourceFunc(func(context.Context, Scheme) (CredentialResult, error) {
				return Presented("token"), nil
			}),
		)
		if !errors.Is(err, ErrInvalidAuthenticatorResult) {
			t.Fatalf("Authenticate() error = %v, want ErrInvalidAuthenticatorResult", err)
		}
	})
}

func TestSchemeValidationAndRestrictiveDefaults(t *testing.T) {
	t.Parallel()

	for _, scheme := range []Scheme{"jwt", "api-key", "oidc_v2", "2fa"} {
		if err := scheme.Validate(); err != nil {
			t.Errorf("Scheme(%q).Validate() error = %v", scheme, err)
		}
	}
	for _, scheme := range []Scheme{"", "JWT", "api key", "oidc/v2"} {
		if err := scheme.Validate(); !errors.Is(err, ErrInvalidScheme) {
			t.Errorf("Scheme(%q).Validate() error = %v, want ErrInvalidScheme", scheme, err)
		}
	}

	_, err := NewManager(ManagerOptions{
		Authenticators: []Authenticator{&stubAuthenticator{scheme: schemeJWT}},
	})
	if !errors.Is(err, ErrNoDefaultSchemes) {
		t.Fatalf("NewManager() error = %v, want ErrNoDefaultSchemes", err)
	}

	manager := newTestManager(t, []Scheme{schemeJWT}, &stubAuthenticator{scheme: schemeJWT})
	if !DefaultSelection().UsesDefault() || !(Selection{}).UsesDefault() {
		t.Fatal("default and zero selections must use restrictive manager defaults")
	}
	if SelectSchemes().UsesDefault() {
		t.Fatal("empty explicit selection unexpectedly uses defaults")
	}
	if err := manager.ValidateSelection(SelectSchemes()); !errors.Is(err, ErrEmptySelection) {
		t.Fatalf("ValidateSelection(empty) error = %v, want ErrEmptySelection", err)
	}
}

func assertRejection(t *testing.T, result Result, wantKind RejectionKind, wantReason SafeReason) {
	t.Helper()
	if !result.Rejected() || result.Status() != ResultStatusRejected {
		t.Fatalf("result status = %s, want rejected", result.Status())
	}
	if result.Authenticated() {
		t.Fatal("rejected result also reported authenticated")
	}
	kind, ok := result.Rejection()
	if !ok || kind != wantKind {
		t.Fatalf("Rejection() = %s, %v, want %s, true", kind, ok, wantKind)
	}
	reason, ok := result.Reason()
	if !ok || reason != wantReason {
		t.Fatalf("Reason() = %q, %v, want %q, true", reason, ok, wantReason)
	}
	if principal, ok := result.Principal(); ok || principal != nil {
		t.Fatalf("Principal() = %#v, %v, want nil, false", principal, ok)
	}
}
