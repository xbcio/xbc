package authentication

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

// ManagerOptions configures an immutable authentication Manager.
type ManagerOptions struct {
	// Authenticators must already be in authentication-domain order. Manager
	// preserves this order; map, bundle, route-selection, and construction order
	// never override it.
	Authenticators []Authenticator

	// DefaultSchemes is the restrictive set used by DefaultSelection and by the
	// zero Selection. It must be non-empty, duplicate-free, and known. Its input
	// order is not behavioral; effective evaluation follows Authenticators.
	DefaultSchemes []Scheme
}

type orderedAuthenticator struct {
	scheme        Scheme
	authenticator Authenticator
}

// Manager applies route scheme selection and ordered first-applicable
// authentication. A Manager is immutable after construction and safe for
// concurrent use when its Authenticators and CredentialSource are safe for
// concurrent use.
type Manager struct {
	ordered        []orderedAuthenticator
	registered     map[Scheme]int
	defaultSet     map[Scheme]struct{}
	defaultOrdered []Scheme
}

// NewManager validates authenticators and a restrictive default selection.
func NewManager(options ManagerOptions) (*Manager, error) {
	if len(options.Authenticators) == 0 {
		return nil, ErrNoAuthenticators
	}

	manager := &Manager{
		ordered:    make([]orderedAuthenticator, 0, len(options.Authenticators)),
		registered: make(map[Scheme]int, len(options.Authenticators)),
	}
	for position, authenticator := range options.Authenticators {
		if isNilLike(authenticator) {
			return nil, fmt.Errorf("%w at ordered position %d", ErrNilAuthenticator, position)
		}
		scheme := authenticator.Scheme()
		if err := scheme.Validate(); err != nil {
			return nil, fmt.Errorf("authenticator at ordered position %d: %w", position, err)
		}
		if previous, exists := manager.registered[scheme]; exists {
			return nil, fmt.Errorf(
				"%w %q in authenticators at ordered positions %d and %d",
				ErrDuplicateScheme,
				scheme,
				previous,
				position,
			)
		}
		manager.registered[scheme] = position
		manager.ordered = append(manager.ordered, orderedAuthenticator{
			scheme:        scheme,
			authenticator: authenticator,
		})
	}

	if len(options.DefaultSchemes) == 0 {
		return nil, ErrNoDefaultSchemes
	}
	manager.defaultSet = make(map[Scheme]struct{}, len(options.DefaultSchemes))
	defaultPositions := make(map[Scheme]int, len(options.DefaultSchemes))
	for position, scheme := range options.DefaultSchemes {
		if err := scheme.Validate(); err != nil {
			return nil, fmt.Errorf("default scheme at position %d: %w", position, err)
		}
		if previous, exists := defaultPositions[scheme]; exists {
			return nil, fmt.Errorf(
				"%w %q in defaults at positions %d and %d",
				ErrDuplicateScheme,
				scheme,
				previous,
				position,
			)
		}
		defaultPositions[scheme] = position
		if _, exists := manager.registered[scheme]; !exists {
			return nil, fmt.Errorf(
				"%w %q in defaults; registered schemes: %s",
				ErrUnknownScheme,
				scheme,
				formatSchemes(manager.Schemes()),
			)
		}
		manager.defaultSet[scheme] = struct{}{}
	}
	for _, entry := range manager.ordered {
		if _, selected := manager.defaultSet[entry.scheme]; selected {
			manager.defaultOrdered = append(manager.defaultOrdered, entry.scheme)
		}
	}

	return manager, nil
}

// Schemes returns all registered schemes in authentication-domain order.
func (m *Manager) Schemes() []Scheme {
	if m == nil {
		return nil
	}
	schemes := make([]Scheme, 0, len(m.ordered))
	for _, entry := range m.ordered {
		schemes = append(schemes, entry.scheme)
	}
	return schemes
}

// DefaultSchemes returns the restrictive default in authentication-domain
// order, regardless of the order in ManagerOptions.DefaultSchemes.
func (m *Manager) DefaultSchemes() []Scheme {
	if m == nil {
		return nil
	}
	return append([]Scheme(nil), m.defaultOrdered...)
}

// Selection is a closed route scheme selection. Its zero value is the
// restrictive manager default, never public access. Public routes bypass
// authentication explicitly in the transport layer.
type Selection struct {
	explicit bool
	schemes  []Scheme
}

// DefaultSelection selects the Manager's restrictive default schemes. It is
// equivalent to the zero Selection.
func DefaultSelection() Selection { return Selection{} }

// SelectSchemes creates an explicit route selection and defensively copies the
// supplied schemes. Manager validation rejects empty, duplicate, invalid, or
// unknown selections.
func SelectSchemes(schemes ...Scheme) Selection {
	return Selection{explicit: true, schemes: append([]Scheme(nil), schemes...)}
}

// UsesDefault reports whether this selection resolves through the Manager's
// restrictive default.
func (s Selection) UsesDefault() bool { return !s.explicit }

// Schemes returns a defensive copy of explicitly selected schemes. It returns
// nil for a default selection because effective defaults belong to Manager.
func (s Selection) Schemes() []Scheme {
	if !s.explicit {
		return nil
	}
	return append([]Scheme(nil), s.schemes...)
}

// ValidateSelection validates a route selection before route freeze. It does
// not invoke credential sources or authenticators.
func (m *Manager) ValidateSelection(selection Selection) error {
	_, err := m.resolve(selection)
	return err
}

// Authenticate applies ordered first-applicable authentication:
//   - resolve default or route-selected schemes in authentication-domain order;
//   - collect every selected CredentialResult exactly once before verification;
//   - reject multiple evidence states (Presented or Malformed) as ambiguous;
//   - reject a sole Malformed result without invoking an Authenticator;
//   - invoke only the Authenticator for a sole Presented credential; and
//   - reject all-Absent input with ordered, de-duplicated challenges.
//
// Rejection is returned as a Result with a nil error. Credential-source and
// Authenticator errors are terminal OperationalError values and never trigger
// fallback to another mechanism.
func (m *Manager) Authenticate(
	ctx context.Context,
	selection Selection,
	source CredentialSource,
) (Result, error) {
	if ctx == nil {
		return Result{}, ErrNilContext
	}
	if isNilLike(source) {
		return Result{}, ErrNilCredentialSource
	}

	selected, err := m.resolve(selection)
	if err != nil {
		return Result{}, err
	}

	var (
		challenges     []Challenge
		seenChallenges = make(map[Challenge]struct{}, len(selected))
		firstRejection *Result
		firstMalformed *Result
	)
	recordChallenge := func(challenge Challenge) {
		if challenge == "" {
			return
		}
		if _, duplicate := seenChallenges[challenge]; duplicate {
			return
		}
		seenChallenges[challenge] = struct{}{}
		challenges = append(challenges, challenge)
	}

	for _, entry := range selected {
		result, sourceErr := source.Credential(ctx, entry.scheme)
		if sourceErr != nil {
			return Result{}, newOperationalError(
				OperationCredentialCollection,
				entry.scheme,
				sourceErr,
			)
		}
		if validationErr := validateCredentialResult(result); validationErr != nil {
			return Result{}, newOperationalError(
				OperationCredentialCollection,
				entry.scheme,
				validationErr,
			)
		}

		recordChallenge(result.challenge)

		switch result.status {
		case CredentialStatusAbsent:
			continue
		case CredentialStatusMalformed:
			if firstMalformed == nil {
				var malformedChallenges []Challenge
				if result.challenge != "" {
					malformedChallenges = []Challenge{result.challenge}
				}
				rejection := managerRejection(
					RejectionMalformedCredential,
					entry.scheme,
					result.reason,
					malformedChallenges,
				)
				firstMalformed = &rejection
			}
			continue
		}

		credential := Credential{value: result.credential}
		authenticatorResult, authenticateErr := entry.authenticator.Authenticate(ctx, credential)
		if authenticateErr != nil {
			return Result{}, newOperationalError(
				OperationAuthentication,
				entry.scheme,
				authenticateErr,
			)
		}
		if validationErr := validateAuthenticatorResult(authenticatorResult); validationErr != nil {
			return Result{}, newOperationalError(
				OperationAuthentication,
				entry.scheme,
				validationErr,
			)
		}
		if authenticatorResult.status == ResultStatusAuthenticated {
			authenticatorResult.scheme = entry.scheme
			return authenticatorResult, nil
		}
		if firstRejection == nil {
			authenticatorResult.scheme = entry.scheme
			firstRejection = &authenticatorResult
		}
	}

	switch {
	case firstRejection != nil:
		return *firstRejection, nil
	case firstMalformed != nil:
		return *firstMalformed, nil
	default:
		return managerRejection(
			RejectionUnauthenticated,
			"",
			ReasonUnauthenticated,
			challenges,
		), nil
	}
}

func (m *Manager) resolve(selection Selection) ([]orderedAuthenticator, error) {
	if m == nil {
		return nil, ErrNoAuthenticators
	}
	if !selection.explicit {
		selected := make([]orderedAuthenticator, 0, len(m.defaultSet))
		for _, entry := range m.ordered {
			if _, exists := m.defaultSet[entry.scheme]; exists {
				selected = append(selected, entry)
			}
		}
		return selected, nil
	}
	if len(selection.schemes) == 0 {
		return nil, ErrEmptySelection
	}

	selectedSet := make(map[Scheme]struct{}, len(selection.schemes))
	positions := make(map[Scheme]int, len(selection.schemes))
	for position, scheme := range selection.schemes {
		if err := scheme.Validate(); err != nil {
			return nil, fmt.Errorf("selected scheme at position %d: %w", position, err)
		}
		if previous, exists := positions[scheme]; exists {
			return nil, fmt.Errorf(
				"%w %q in route selection at positions %d and %d",
				ErrDuplicateScheme,
				scheme,
				previous,
				position,
			)
		}
		positions[scheme] = position
		if _, exists := m.registered[scheme]; !exists {
			return nil, fmt.Errorf(
				"%w %q in route selection; registered schemes: %s",
				ErrUnknownScheme,
				scheme,
				formatSchemes(m.Schemes()),
			)
		}
		selectedSet[scheme] = struct{}{}
	}

	selected := make([]orderedAuthenticator, 0, len(selectedSet))
	for _, entry := range m.ordered {
		if _, exists := selectedSet[entry.scheme]; exists {
			selected = append(selected, entry)
		}
	}
	return selected, nil
}

func managerRejection(
	kind RejectionKind,
	scheme Scheme,
	reason SafeReason,
	challenges []Challenge,
) Result {
	return Result{
		status:     ResultStatusRejected,
		rejection:  kind,
		scheme:     scheme,
		reason:     reason,
		challenges: append([]Challenge(nil), challenges...),
	}
}

func validateCredentialResult(result CredentialResult) error {
	switch result.status {
	case CredentialStatusAbsent:
		if result.credential != nil || result.reason != "" {
			return ErrInvalidCredentialResult
		}
	case CredentialStatusPresented:
		if isNilLike(result.credential) || result.reason != "" || result.challenge != "" {
			return ErrInvalidCredentialResult
		}
	case CredentialStatusMalformed:
		if result.credential != nil || result.reason == "" {
			return ErrInvalidCredentialResult
		}
	default:
		return ErrInvalidCredentialResult
	}
	return nil
}

func validateAuthenticatorResult(result Result) error {
	switch result.status {
	case ResultStatusAuthenticated:
		if isNilLike(result.principal) || result.rejection != 0 || result.scheme != "" ||
			result.reason != "" || len(result.challenges) != 0 {
			return ErrInvalidAuthenticatorResult
		}
	case ResultStatusRejected:
		if result.principal != nil || result.rejection != RejectionInvalidCredential ||
			result.scheme != "" || result.reason == "" || len(result.challenges) > 1 {
			return ErrInvalidAuthenticatorResult
		}
	default:
		return ErrInvalidAuthenticatorResult
	}
	return nil
}

func isNilLike(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func formatSchemes(schemes []Scheme) string {
	formatted := make([]string, 0, len(schemes))
	for _, scheme := range schemes {
		formatted = append(formatted, fmt.Sprintf("%q", scheme))
	}
	return strings.Join(formatted, ", ")
}
