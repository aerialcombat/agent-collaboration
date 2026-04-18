// Package envelope defines the canonical JSON schema for the peer-inbox
// delivery envelope (§5.1 of plans/v3.x-topic-3-implementation-scope.md)
// AND the shared <peer-inbox>...</peer-inbox> text serializer that the
// two v0 consumers call.
//
// This is the internal data structure that both v0 consumers of the
// peer-inbox serializer share (§5.2):
//
//  1. The Option J hook path — go/cmd/hook builds an Envelope from
//     claimed inbox rows and renders the <peer-inbox>...</peer-inbox>
//     text block into hookSpecificOutput.additionalContext.
//  2. The daemon-mode prompt-injection path (commit 7 of Topic 3 v0) —
//     the agent-collab daemon (go/cmd/daemon) builds an Envelope for a
//     freshly-claimed batch, passes it through RenderText here, and
//     injects the resulting block as the spawned CLI's user-prompt
//     input.
//
// Both consumers must produce byte-identical text for a given batch.
// The shared serializer (RenderText) IS the enforcement — the hook
// binary's formatHookBlock and the daemon binary's prompt-injection
// both call RenderText on the same Envelope shape. tests/envelope-
// round-trip.sh asserts byte-parity across consumers; the hoist of
// the renderer from go/cmd/hook/main.go into this package (commit 7)
// makes the single-source-of-truth structural rather than discipline-
// dependent.
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

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultHookBlockBudgetBytes is the fallback budget used by RenderText
// when the caller supplies 0 or a negative value. Mirrors the Python
// HOOK_BLOCK_BUDGET default (4 KiB) in scripts/peer-inbox-db.py.
//
// Budget counts UTF-8 bytes — the idle-birch #3 ruling aligns both
// runtimes to bytes because HOOK_BLOCK_BUDGET is a 4 KiB transport
// budget and bytes are the real constraint surface.
const DefaultHookBlockBudgetBytes = 4 * 1024

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

// RenderText serializes an Envelope into the Option J
// <peer-inbox>...</peer-inbox> text block. The text format is fixed
// by tests/hook-parity.sh fixtures; any divergence is a path (b) scope
// creep per §5.2 and requires re-scoping before landing.
//
// budgetBytes caps the rendered block size (byte count). Callers pass
// 0 to accept DefaultHookBlockBudgetBytes. When truncation fires, a
// "+N more messages truncated" tail marker is appended before the
// closing tag — byte-identical to the pre-hoist renderer in
// go/cmd/hook/main.go so tests/hook-parity.sh + tests/envelope-
// round-trip.sh stay green.
//
// v0 emits only the envelope's Messages; To / ContinuitySummary /
// State / ContentStop are not yet surfaced in the hook-text rendering.
// The daemon prompt-injection consumer uses the same text byte-for-
// byte for the message-only case; future format extensions for the
// optional fields are a path (b) decision out of scope here.
func RenderText(env Envelope, budgetBytes int) string {
	if len(env.Messages) == 0 {
		return ""
	}
	if budgetBytes <= 0 {
		budgetBytes = DefaultHookBlockBudgetBytes
	}

	// labels = sorted({ m.FromLabel for m in env.Messages })
	labelSet := make(map[string]struct{}, len(env.Messages))
	for _, m := range env.Messages {
		labelSet[m.FromLabel] = struct{}{}
	}
	labels := make([]string, 0, len(labelSet))
	for l := range labelSet {
		labels = append(labels, l)
	}
	sort.Strings(labels)

	var b strings.Builder
	header := fmt.Sprintf(
		`<peer-inbox messages="%d" from-session-labels="%s">`,
		len(env.Messages), strings.Join(labels, ","),
	)
	b.WriteString(header)
	// Reserve space for the closing tag so the final block fits within
	// budget. Truncation message (~60 bytes when triggered) is
	// unaccounted headroom; HOOK_BLOCK_BUDGET is a 4 KiB ceiling
	// against ~hundreds-of-bytes messages, so the approximation is
	// safe (matches the pre-hoist behavior in go/cmd/hook/main.go).
	const closingTag = "</peer-inbox>"
	used := len(header) + len(closingTag)

	included, truncated := 0, 0
	for _, m := range env.Messages {
		entry := fmt.Sprintf("\n[%s @ %s]\n%s\n", m.FromLabel, m.CreatedAt, m.Body)
		if used+len(entry) > budgetBytes && included > 0 {
			truncated = len(env.Messages) - included
			break
		}
		b.WriteString(entry)
		used += len(entry)
		included++
	}
	if truncated > 0 {
		fmt.Fprintf(&b,
			"\n[+%d more messages truncated; run agent-collab peer receive to view]\n",
			truncated,
		)
	}
	b.WriteString("</peer-inbox>")
	return b.String()
}
