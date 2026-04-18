// Package localtrust implements the trivial v1 Auth: every AuthContext is
// trusted. Scoped to the single-machine deployment where the OS already
// provides the trust boundary (filesystem permissions on
// ~/.agent-collab/sessions.db and the Unix socket).
//
// Swapped out for an OIDCBearer impl when cloud ships in v2. That swap is a
// one-line wiring change in the caller — the v1 scope's whole point.
package localtrust

import (
	"context"

	"agent-collaboration/go/pkg/auth"
	pb "agent-collaboration/go/pkg/protocol"
)

// LocalTrust accepts every AuthContext.
type LocalTrust struct{}

// New returns a zero-configuration LocalTrust.
func New() *LocalTrust { return &LocalTrust{} }

// Verify returns Trusted=true for every caller. Reason="local_trust" so
// slog logs show which Auth impl made the decision.
func (*LocalTrust) Verify(ctx context.Context, _ *pb.AuthContext) auth.Decision {
	return auth.Decision{Trusted: true, Reason: "local_trust"}
}

// Compile-time assertion: LocalTrust implements auth.Auth.
var _ auth.Auth = (*LocalTrust)(nil)
