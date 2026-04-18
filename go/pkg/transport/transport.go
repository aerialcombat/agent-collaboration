// Package transport declares the Transport interface for peer-inbox wire
// messaging. v1 has one implementation (UnixSocketTransport, landing in W2
// per plans/v3.0-go-cloud-ready-scoping.md); v2 adds GRPCClient and
// GRPCServer behind the same interface. Transport callers speak only in
// protobuf Envelope bytes — never JSON, never anything else — so the
// cloud-ship step is purely additive implementations behind this boundary.
//
// Exported here in W1 so `go vet` and the Store+Auth wiring have a stable
// symbol. No implementation ships this week.
package transport

import (
	"context"

	pb "agent-collaboration/go/pkg/protocol"
)

// Transport moves Envelope bytes between peers. Implementations MUST
// marshal/unmarshal via google.golang.org/protobuf/proto — never JSON.
// Callers (CLI, daemon) never see raw bytes.
type Transport interface {
	// Send encodes the envelope with the v1 framing (4-byte magic +
	// 4-byte big-endian length + protobuf payload) and writes it to the
	// peer identified by `peerLabel`. Blocks until the write completes or
	// ctx is cancelled.
	Send(ctx context.Context, peerLabel string, envelope *pb.Envelope) error

	// Recv reads one Envelope off the wire. Implementations MUST reject
	// frames whose first 4 bytes do not match the v1 magic and return a
	// structured error (`ErrInvalidFrame`) that carries size + first-bytes
	// diagnostic context — callers log the structured fields and move on.
	Recv(ctx context.Context) (*pb.Envelope, error)

	// Close releases the underlying connection.
	Close() error
}
