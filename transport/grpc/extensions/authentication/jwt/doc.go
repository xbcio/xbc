// Package jwt is the gRPC credential extractor for XBC's HMAC JWT
// authentication.
//
// Verification, signing, and configuration live in the protocol-neutral
// github.com/xbcio/xbc/extensions/authentication/jwt. This package only reads
// the bearer token from the metadata entry that authenticator is configured
// with -- its header name, lower-cased as gRPC delivers it, so authorization
// by default -- and hands it to the gRPC Server's built-in authentication
// stage. It has no configuration section of its own: configuring plugins.jwt
// activates both halves, and Bundle selects both. It is a module of its own so
// the JWT library never enters transport/grpc's dependency closure.
//
// # Usage
//
// Configure plugins.jwt.secret with at least 32 bytes, select Bundle (or blank
// import the autoload package), and read the identity the stage publishes.
// Verified claims travel as authentication.Principal.Attributes:
//
//	func (s *profiles) Get(ctx context.Context, _ *pb.GetRequest) (*pb.Profile, error) {
//		principal, ok := grpc.PrincipalFromContext(ctx)
//		if !ok {
//			return nil, status.Error(codes.Unauthenticated, "unauthenticated")
//		}
//		role, _ := principal.Attributes["role"].(string)
//		return &pb.Profile{Subject: principal.Subject, Role: role}, nil
//	}
//
// A client sends the token as authorization metadata, for example with
// metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token).
// Issue tokens with the neutral plugin's Sign and SignWithTTL.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/transport/grpc/extensions/authentication/jwt/autoload.
package jwt
