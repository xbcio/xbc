// Package rbac provides protocol-neutral business authorization semantics over
// one exact policy Backend. It owns input normalization, administrator
// short-circuiting, AND/OR checks, and convergent role and permission
// management. Applications continue to own user, role, and permission tables,
// subject and role encoding, management CRUD, audit, and cross-domain
// transactions.
//
// # Usage
//
// Compose Bundle explicitly with a compatible backend exporter. The default
// reference selects casbin[default]:
//
//	app, err := xbc.New(xbc.WithBundles(
//		casbin.Bundle(),
//		rbac.Bundle(),
//	))
//
// Business and transport code should depend on Manager rather than a concrete
// policy engine. This package does not import or define HTTP, gRPC, or other
// transport behavior. Transport-specific adapters can consume Manager without
// owning RBAC policy or lifecycle. Ordinary imports and Bundle calls are
// side-effect free; executables that intentionally use process-wide optional
// composition may import security/rbac/autoload.
package rbac
