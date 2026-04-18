// Package envelope defines the canonical JSON schema for the peer-inbox
// delivery envelope (§5.1 of plans/v3.x-topic-3-implementation-scope.md).
//
// This is the internal data structure that both v0 consumers of the
// peer-inbox serializer share (§5.2):
//
//  1. The Option J hook path — go/cmd/hook builds an Envelope from
//     claimed inbox rows and renders the <peer-inbox>...</peer-inbox>
//     text block into hookSpecificOutput.additionalContext.
//  2. The daemon-mode prompt-injection path (commit 7 of Topic 3 v0) —
//     the agent-collab daemon builds an Envelope for a freshly-claimed
//     batch, passes it through the shared text serializer, and injects
//     the resulting block as the spawned CLI's user-prompt input.
//
// Both consumers must produce byte-identical text for a given batch; a
// shared envelope type + shared serializer is the enforcement. The text
// renderer itself stays in go/cmd/hook/main.go for now (it is called by
// only one v0 consumer today — the daemon in commit 7 will import this
// package + its own copy of the renderer, or the renderer will hoist
// into this package. The scope of commit 5 is the schema; the hoist
// lands with the daemon if needed).
//
// The schema mirrors the Python build_peer_inbox_envelope dict in
// scripts/peer-inbox-db.py — JSON tags keep a Go-serialized envelope
// byte-equal to a Python-serialized one under
// json.Marshal / json.Dump(ensure_ascii=False) respectively. This is
// asserted by tests/envelope-round-trip.sh at shell level.
//
// v0 does NOT include an ack.batch_id field per §5.1 + §7.3 (alpha #8 +
// reason endorse): daemons enforce single-batch-at-a-time semantics, so
// completion correlates through claim_owner alone on the inbox table.
// A v1+ schema can add claimed_batch_id additively when / if
// concurrent-batch-per-daemon semantics become desirable.
package envelope

// Addressee is the `to` field: who a batch is delivered to.
//
// `cwd` + `label` is the presentation key used by peer-inbox routing
// today. `workspace_id` is the forward-compat hook for the v2 delivery-
// key migration (item 6 of the scope doc). Omitted from JSON when
// unset; Go's zero-value string + `omitempty` handles this.
type Addressee struct {
	CWD         string `json:"cwd"`
	Label       string `json:"label"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// Message is one claimed inbox row as surfaced to a consumer.
//
// Field-for-field parity with the Python envelope's messages[] entry.
// RoomKey is present on daemon-side builders (rows include it when
// SELECTed); hook-side ReadUnread omits it today, so callers rendering
// from hook rows leave it empty and `omitempty` keeps it out of the
// JSON.
type Message struct {
	ID        int64  `json:"id"`
	FromCWD   string `json:"from_cwd"`
	FromLabel string `json:"from_label"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	RoomKey   string `json:"room_key,omitempty"`
}

// Envelope is the canonical JSON envelope — one batch, addressed to
// one recipient, optionally annotated with continuity summary +
// termination-stack signals.
//
// `To` is a pointer so hook-side callers (which resolve the recipient
// implicitly via ResolveSelf and therefore don't need to embed it in
// the envelope) can pass nil to omit the field entirely. Daemon-side
// callers set it because they know the addressee up-front.
//
// `ContinuityStateStop` wording: `State` is the unified envelope state
// (§7.1 item 4 — "open" | "closed"), `ContentStop` is Layer 1's
// content-stop signal (§6), kept distinct per scope-doc §5.1 guidance.
// Both are pointers so callers can omit them; a bare zero-value "" /
// false would be ambiguous vs "unset".
type Envelope struct {
	To                *Addressee `json:"to,omitempty"`
	Messages          []Message  `json:"messages"`
	ContinuitySummary *string    `json:"continuity_summary,omitempty"`
	State             *string    `json:"state,omitempty"`
	ContentStop       *bool      `json:"content_stop,omitempty"`
}

// BuildFromHookRows constructs an Envelope from a slice of messages
// produced by the hook hot-path (store.InboxMessage via ReadUnread).
// Callers from the hook path pass to=nil; daemon-side callers that know
// the recipient pass a populated Addressee.
//
// Kept signature-minimal on purpose — the hook path is the hot loop.
// Callers needing richer envelopes (continuity summary, state flags)
// can build the struct literal directly.
func BuildFromHookRows(
	messages []Message,
	to *Addressee,
) Envelope {
	return Envelope{
		To:       to,
		Messages: messages,
	}
}
