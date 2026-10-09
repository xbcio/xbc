package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Mount registers h as the handler for the subtree rooted at relativePath, on
// every method in anyMethods.
//
// It is for the handler that arrives with its own routing: an integration
// library, a file server, another framework's engine. Such a handler owns its
// paths and its matching, so wrapping it as a single Handler would mean
// re-implementing its routing inside a route that was already declared -- the
// prefix says where it starts, and everything beneath it belongs to h.
//
// The request h receives has the mounted prefix stripped, exactly as
// http.StripPrefix defines it: a request to "/flow/status" under a mount at
// "/flow" arrives as "/status", and a request to the prefix itself arrives
// with an empty path. The strip is applied to a copy of the request -- the
// request every framework middleware sees keeps its original path, so access
// logs, metrics, and the header machinery still name what the client asked
// for, and h cannot corrupt them by rewriting the URL it was handed.
//
// A mount is a route declaration first: it writes one RouteInfo per method
// into the frozen table, all with Mounted true and Path set to the prefix.
// The group-level Perm/Auth defaults and the middleware on this Router apply
// to every row, and the returned *Route handle carries metadata to all of
// them at once, like Match and Any. Three framework behaviors follow from
// that registration rather than from any special case:
//
//   - CurrentRoute answers with the mount row anywhere inside the subtree, so
//     authentication resolves a mounted request the way it resolves a route's:
//     web.security.default decides wherever nothing is declared, and an
//     application-level wildcard rule naming the prefix ("GET /flow/**")
//     outranks a route-level declaration made here. A rule naming a path
//     inside the subtree can never match: policy resolves against the route
//     table's Path, and the table knows only the prefix.
//
//   - The in-flight gate admits mounted requests like any other route's, and
//     Unmetered is refused on a mount: a gate exemption is keyed by the exact
//     method and path, which a subtree does not have, so freeze reports the
//     combination instead of leaving it silently metered.
//
//   - A request whose method was not mounted reaches the framework's 405 with
//     Allow rather than h; the subtree is mounted for the anyMethods list and
//     for no other method.
//
// Registration refuses overlaps. A mount is refused when its subtree holds an
// existing registration on the same method -- a route at the prefix, a route
// under it, another mount anywhere in it -- and when an existing pattern
// route reaches the prefix: a path carrying ":" or "*" claims the subtree
// below its last literal segment, so no mount at or beneath that prefix can
// share the method with it. A mount strictly below a plain route stays legal:
// that route names one path, and the mount's subtree starts beneath it. A
// route registered at or under a mount is refused the same way, since a mount
// owns its whole subtree. The refusal is xbc's rather than the engine's
// because engines disagree about the overlap -- gin panics on a conflicting
// subtree without naming either side, ServeMux accepts one silently -- and a
// conflict resolved differently per engine is worse than an explicit refusal.
// The check is per method: the same prefix under a different method is a
// distinct registration in every engine xbc ships.
//
// The prefix is a literal, non-root path without a trailing slash. The root
// is refused because a mount there would own every path, conflicting with
// every other route and displacing the unmatched-request chains that produce
// the framework's own 404 and 405.
func (r *Router) Mount(relativePath string, h http.Handler) *Route {
	r.requireMutable()
	if h == nil {
		panic("xbc: Mount requires a non-nil http.Handler")
	}
	fullPath := mountPrefix(r.basePath, relativePath)

	// Every method is checked before anything is registered, so a conflict
	// found on the last method cannot leave the earlier ones mounted.
	for _, method := range anyMethods {
		if existing, conflict := conflictingRegistration(method, fullPath, true, *r.routes); conflict {
			panic(fmt.Sprintf(
				"xbc: mount %s %s overlaps %s registered on the same method, and a mount owns its whole subtree",
				method, fullPath, describeRoute(existing)))
		}
	}

	handler := http.StripPrefix(fullPath, h)
	terminal := func(_ context.Context, c *Ctx) error {
		handler.ServeHTTP(c.Writer(), c.Request())
		return nil
	}

	indexes := make([]int, 0, len(anyMethods))
	for _, method := range anyMethods {
		chain := appendChain([]Handler{recordCurrentRoute(method, fullPath, r.frozen, r.index)}, r.handlers...)
		chain = appendChain(chain, terminal)
		r.engine.Mount(method, fullPath, chain)
		*r.routes = append(*r.routes, RouteInfo{
			Method:  method,
			Path:    fullPath,
			Auth:    cloneAuthPolicy(r.defaultAuth),
			Perm:    r.defaultPerm,
			Mounted: true,
		})
		indexes = append(indexes, len(*r.routes)-1)
	}
	return &Route{Router: r, indexes: indexes}
}

// mountPrefix resolves the absolute prefix a Mount call claims and refuses
// the shapes a prefix cannot have. Each refusal stands for a meaning the
// literal form does not already carry: the root, which belongs to the
// framework's own routes and unmatched-request chains; a trailing slash,
// which names the same subtree while registering the subtree wildcard behind
// a doubled slash; and a ":" or "*", which is engine routing syntax rather
// than a literal path.
func mountPrefix(basePath, relativePath string) string {
	fullPath := joinPaths(basePath, relativePath)
	switch {
	case fullPath == "/":
		panic("xbc: Mount cannot claim the service root, where every other route and the framework's unmatched-request chains live")
	case strings.HasSuffix(fullPath, "/"):
		panic(fmt.Sprintf("xbc: Mount requires a prefix without a trailing slash, got %q", fullPath))
	case strings.ContainsAny(fullPath, ":*"):
		panic(fmt.Sprintf("xbc: Mount requires a literal prefix, but %q contains %q or %q", fullPath, ":", "*"))
	}
	return fullPath
}

// conflictingRegistration reports whether the route table already holds an
// entry this registration cannot coexist with, and returns that entry so the
// panic can name both sides. mounted says whether the incoming registration
// is a mount.
//
// The predicate only ever fires on an overlap a mount is part of. Overlaps
// that no mount is part of -- two plain routes on one path, two pattern routes
// whose trees collide -- stay the engine's own registration error: those are
// one engine's syntax presented to it, while an overlap with a mount is the
// one the engines disagree about.
//
// A mount holds a subtree. So does a route whose path carries engine pattern
// syntax (":" or "*"), from the last literal segment before the pattern:
// every engine xbc ships matches a pattern at a fixed position in its tree,
// so a mount below the pattern's literal prefix collides with it however
// unrelated the two path strings look. Two claims overlap when both hold
// subtrees and either contains the other, or when a subtree holds the other
// side's single path. A plain route above a mount stays legal, while a
// pattern route is compared at that same coarse grain on purpose: naming a
// path the pattern would not really match is a refusal an author can read and
// rename, while missing one is an engine panic that names neither side.
//
// The comparison is on path segments, never on raw characters: "/flow" owns
// "/flow/x" but not "/flowx", which is exactly the boundary both adapters'
// matchers draw.
func conflictingRegistration(method, fullPath string, mounted bool, routes []RouteInfo) (RouteInfo, bool) {
	incomingBase, incomingSubtree := registrationClaim(fullPath, mounted)
	for _, existing := range routes {
		if existing.Method != method {
			continue
		}
		if !mounted && !existing.Mounted {
			continue
		}
		existingBase, existingSubtree := registrationClaim(existing.Path, existing.Mounted)
		if registrationOverlap(existingBase, existingSubtree, incomingBase, incomingSubtree) {
			return existing, true
		}
	}
	return RouteInfo{}, false
}

// registrationClaim returns what a registration holds on its method: the base
// its matching branches from, and whether it holds a whole subtree there
// rather than that single path. A mount holds its subtree. A route whose path
// carries pattern syntax holds the subtree below its literal prefix: the
// pattern picks the paths inside it, which is what makes the two registrations
// each other's business rather than a comparison of path strings.
func registrationClaim(path string, mounted bool) (string, bool) {
	if mounted {
		return path, true
	}
	base := literalPrefix(path)
	return base, base != path
}

// literalPrefix is the part of a registration path that stays literal: the
// path before the segment carrying the first ":" or "*", which is the last
// point every engine's tree still branches at a fixed position. A path with
// no pattern segment is its own prefix; a pattern in the first segment makes
// the root the prefix, reaching every other path.
func literalPrefix(path string) string {
	i := strings.IndexAny(path, ":*")
	if i < 0 {
		return path
	}
	if slash := strings.LastIndex(path[:i], "/"); slash >= 0 {
		return path[:slash]
	}
	return ""
}

// registrationOverlap reports whether two registrations' claims on one method
// overlap: two subtrees overlap when either contains the other, and a subtree
// and a single path overlap when the subtree contains the path.
func registrationOverlap(aBase string, aSubtree bool, bBase string, bSubtree bool) bool {
	switch {
	case aSubtree && bSubtree:
		return subtreeContains(aBase, bBase) || subtreeContains(bBase, aBase)
	case aSubtree:
		return subtreeContains(aBase, bBase)
	case bSubtree:
		return subtreeContains(bBase, aBase)
	}
	return false
}

// subtreeContains reports whether the subtree rooted at prefix holds p:
// prefix itself or a path below it. The root prefix holds every absolute
// path, which is what a pattern in a path's first segment claims.
func subtreeContains(prefix, p string) bool {
	return p == prefix || pathBelow(prefix, p)
}

// pathBelow reports whether p lies strictly below prefix on a path-segment
// boundary: "/flow/x" is below "/flow", "/flowx" is not. The boundary is
// always prefix+"/", so the root prefix ("") is below every absolute path and
// no path is below the root.
func pathBelow(prefix, p string) bool {
	return strings.HasPrefix(p, prefix+"/")
}

// describeRoute names one route-table entry the way the registration
// diagnostics do, so a conflict message says whether the other side was a
// mount or a single-path route.
func describeRoute(route RouteInfo) string {
	if route.Mounted {
		return fmt.Sprintf("mount %s %s", route.Method, route.Path)
	}
	return fmt.Sprintf("route %s %s", route.Method, route.Path)
}
