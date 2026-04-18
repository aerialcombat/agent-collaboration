// Package auth declares the Auth interface for peer-inbox identity
// verification. v1 ships one implementation (localtrust.LocalTrust), which
// always returns trusted=true for any (workspace_id, user_id, session_label).
// v2 will add an OIDCBearer impl that verifies a JWT against an OIDC issuer.
//
// Callers — today only the hook binary — accept an Auth interface so the
// cloud-ship step is purely additive: wire in a different impl, no hot-path
// changes.
package auth

import (
	"context"

	pb "agent-collaboration/go/pkg/protocol"
)

// Decision carries the outcome of verifying an AuthContext. `Reason` is a
// short machine-readable code ("local_trust", "missing_token", "expired",
// "workspace_mismatch") for slog observability.
type Decision struct {
	Trusted bool
	Reason  string
}

// Auth verifies the AuthContext carried in an inbound Envelope.
type Auth interface {
	// Verify is called on every inbound Envelope after framing has been
	// validated. Implementations SHOULD be fast — this sits on the hot
	// path and a slow verify pushes the hook above the 10ms budget.
	Verify(ctx context.Context, auth *pb.AuthContext) Decision
}
