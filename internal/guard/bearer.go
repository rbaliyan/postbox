// Package guard provides SecurityGuard implementations for the mailbox gRPC server.
package guard

import (
	"context"
	"strings"

	mbxserver "github.com/rbaliyan/mailbox/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// BearerGuard authenticates gRPC callers by validating a static Bearer token
// in the "authorization" metadata key (case-insensitive header name).
// All authenticated callers are granted full access — suitable for
// service-to-service use where network segmentation handles multi-tenant isolation.
//
// Configure via --grpc-auth-token or POSTBOX_GRPC_AUTH_TOKEN. Omitting the
// token (empty string) falls back to AllowAll behaviour with a logged warning.
type BearerGuard struct {
	token string
}

// NewBearer returns a SecurityGuard that accepts only callers presenting token
// in the Authorization metadata field. The "Bearer " prefix is optional.
// Panics if token is empty — use AllowAll() explicitly for dev/test.
func NewBearer(token string) *BearerGuard {
	if token == "" {
		panic("guard: bearer token must not be empty — use mbxserver.AllowAll() for development")
	}
	return &BearerGuard{token: token}
}

// Authenticate implements mbxserver.SecurityGuard.
func (g *BearerGuard) Authenticate(ctx context.Context) (mbxserver.Identity, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing request metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "authorization header required")
	}
	tok := strings.TrimPrefix(vals[0], "Bearer ")
	if tok != g.token {
		return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
	}
	return bearerIdentity{}, nil
}

// Authorize implements mbxserver.SecurityGuard. Grants all actions to any
// successfully authenticated caller (single shared secret = single trust level).
func (g *BearerGuard) Authorize(_ context.Context, _ mbxserver.Identity, _ mbxserver.Action, _ mbxserver.Resource) (mbxserver.Decision, error) {
	return mbxserver.Decision{Allowed: true}, nil
}

type bearerIdentity struct{}

func (bearerIdentity) UserID() string         { return "service" }
func (bearerIdentity) Claims() map[string]any { return nil }
