// Package tenant resolves a request tenant only from verified authentication
// facts. The optional tenant header is an untrusted selector and can never add
// membership not already proven by the active Resolver.
//
// # Usage
//
// The built-in resolver reads tenant_id and tenant_ids from the verified Web
// principal. For database-backed membership, inject a Resolver that checks the
// principal subject before accepting the requested header value:
//
//	func newTenancy(memberships map[string]map[string]bool) *tenant.Plugin {
//		resolver := tenant.ResolverFunc(func(
//			ctx context.Context,
//			principal web.Principal,
//			requestedID string,
//		) (tenant.Tenant, bool, error) {
//			if err := ctx.Err(); err != nil {
//				return tenant.Tenant{}, false, err
//			}
//			allowed := memberships[principal.Subject]
//			if requestedID == "" {
//				if len(allowed) != 1 {
//					return tenant.Tenant{}, false, nil
//				}
//				for id := range allowed {
//					requestedID = id
//				}
//			}
//			if !allowed[requestedID] {
//				return tenant.Tenant{}, false, nil
//			}
//			return tenant.Tenant{ID: requestedID}, true, nil
//		})
//		return tenant.New(tenant.WithResolver(resolver))
//	}
//
// Downstream handlers consume only the tenant published by the middleware:
//
//	resolved, ok := tenant.Current(c)
//	if !ok {
//		c.AbortWithStatus(http.StatusForbidden)
//		return
//	}
//	c.JSON(http.StatusOK, gin.H{"tenant_id": resolved.ID})
//
// Never pass an unchecked X-Tenant-ID value to Set or return it from a custom
// Resolver without proving membership. Bundle and ordinary imports are
// side-effect free. Applications that prefer xbc.Run may opt into
// process-global composition through this package's autoload leaf.
package tenant
