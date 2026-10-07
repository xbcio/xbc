package authentication

// Principal is the protocol-neutral result of successful authentication.
// Authenticators produce one Principal; transports publish it on the request,
// and authorization, auditing, and other business logic consume it without
// importing a concrete JWT, API-key, or session implementation.
//
// Subject is the stable policy/audit identity. AuthMethod identifies how the
// request authenticated (for example "jwt" or "apikey"). Attributes are
// optional verified facts; a transport publishing a Principal must give
// callers defensive map copies so they cannot replace another middleware's
// top-level values accidentally -- see transport/web's SetPrincipal and
// CurrentPrincipal.
type Principal struct {
	Subject    string
	AuthMethod string
	Attributes map[string]any
}
