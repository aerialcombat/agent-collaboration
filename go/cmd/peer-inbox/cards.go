package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	sqlitestore "agent-collaboration/go/pkg/store/sqlite"
)

// Card CLI verbs (v3.9). Output contract is `--format json|plain`, the
// same choice set peer-receive and session-list use. The MCP server
// (peer-inbox-channel.py) wraps these verbs, so any shape change here
// has to be mirrored in the tool schemas.

const cardsDefaultTimeout = 15 * time.Second

func cardCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cardsDefaultTimeout)
}

// runCardCreate — card-create --pair-key K --title T [--body B]
// [--needs-role R] [--created-by LABEL] [--priority N] [--tags JSON]
// [--context-refs JSON] [--format json|plain]
func runCardCreate(args []string) int {
	fs := flag.NewFlagSet("card-create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "room / board scope (required)")
	title := fs.String("title", "", "card title (required)")
	body := fs.String("body", "", "markdown body / spec")
	needsRole := fs.String("needs-role", "", "role-based claim target (e.g. ios, qa, review)")
	createdBy := fs.String("created-by", "", "label of the creator (required)")
	priority := fs.Int("priority", 0, "-1 low, 0 normal, 1 high")
	tags := fs.String("tags", "", "JSON array of tags (e.g. [\"feed-card-epic\"])")
	ctxRefs := fs.String("context-refs", "", "JSON object {msg_ids:[], files:[], urls:[], cards:[]}")
	kind := fs.String("kind", sqlitestore.CardKindTask, "card kind: task (default) or epic")
	splittable := fs.Bool("splittable", false, "if true, worker prompt is given the mid-session split addendum")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pairKey == "" || *title == "" || *createdBy == "" {
		fmt.Fprintln(os.Stderr, "card-create: --pair-key, --title, --created-by required")
		return exitUsage
	}
	if *format != "plain" && *format != "json" {
		fmt.Fprintf(os.Stderr, "card-create: --format must be plain or json\n")
		return exitUsage
	}
	if *kind != sqlitestore.CardKindTask && *kind != sqlitestore.CardKindEpic {
		fmt.Fprintln(os.Stderr, "card-create: --kind must be 'task' or 'epic'")
		return exitUsage
	}
	if *tags != "" && !isValidJSON(*tags) {
		fmt.Fprintln(os.Stderr, "card-create: --tags must be valid JSON")
		return exitUsage
	}
	if *ctxRefs != "" && !isValidJSON(*ctxRefs) {
		fmt.Fprintln(os.Stderr, "card-create: --context-refs must be valid JSON")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-create: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	card, err := st.CreateCard(ctx, sqlitestore.CreateCardParams{
		PairKey:     *pairKey,
		HomeHost:    sqlitestore.SelfHost(),
		Title:       *title,
		Body:        *body,
		NeedsRole:   *needsRole,
		CreatedBy:   *createdBy,
		Priority:    *priority,
		Tags:        *tags,
		ContextRefs: *ctxRefs,
		Kind:        *kind,
		Splittable:  *splittable,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-create: %v\n", err)
		return exitInternal
	}
	return emitCard(card, *format)
}

// runCardList — card-list [--pair-key K] [--status S] [--needs-role R]
// [--claimed-by L] [--ready-only] [--blocked-only] [--limit N] [--format]
func runCardList(args []string) int {
	fs := flag.NewFlagSet("card-list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pairKey := fs.String("pair-key", "", "scope to one room")
	status := fs.String("status", "", "filter by status")
	needsRole := fs.String("needs-role", "", "filter by needs_role")
	claimedBy := fs.String("claimed-by", "", "filter by claimed_by label")
	readyOnly := fs.Bool("ready-only", false, "status=todo AND no open blockers")
	blockedOnly := fs.Bool("blocked-only", false, "status=todo AND >=1 open blocker")
	limit := fs.Int("limit", 0, "max rows (0 = all)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *format != "plain" && *format != "json" {
		fmt.Fprintln(os.Stderr, "card-list: --format must be plain or json")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-list: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	cards, err := st.ListCards(ctx, sqlitestore.CardListFilter{
		PairKey:     *pairKey,
		Status:      *status,
		NeedsRole:   *needsRole,
		ClaimedBy:   *claimedBy,
		ReadyOnly:   *readyOnly,
		BlockedOnly: *blockedOnly,
		Limit:       *limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-list: %v\n", err)
		return exitInternal
	}
	return emitCards(cards, *format)
}

// runCardGet — card-get --card N (alias: --id) [--format]
func runCardGet(args []string) int {
	fs := flag.NewFlagSet("card-get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 {
		fmt.Fprintln(os.Stderr, "card-get: --card required")
		return exitUsage
	}
	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-get: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	card, err := st.GetCard(ctx, *id)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		fmt.Fprintf(os.Stderr, "card-get: not found: %d\n", *id)
		return exitNotFound
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-get: %v\n", err)
		return exitInternal
	}
	return emitCard(card, *format)
}

// runCardClaim — card-claim --card N (alias: --id) --as LABEL [--force] [--format]
func runCardClaim(args []string) int {
	fs := flag.NewFlagSet("card-claim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	label := fs.String("as", "", "claimer label (required)")
	force := fs.Bool("force", false, "override prior claim by a different label")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 || *label == "" {
		fmt.Fprintln(os.Stderr, "card-claim: --card and --as required")
		return exitUsage
	}
	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-claim: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	card, err := st.ClaimCard(ctx, *id, *label, *force)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		fmt.Fprintf(os.Stderr, "card-claim: not found: %d\n", *id)
		return exitNotFound
	}
	if errors.Is(err, sqlitestore.ErrCardAlreadyClaimed) {
		fmt.Fprintf(os.Stderr, "card-claim: %v (use --force to override)\n", err)
		return exitDataErr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-claim: %v\n", err)
		return exitInternal
	}
	return emitCard(card, *format)
}

// runCardUpdateStatus — card-update-status --card N (alias: --id) --status S [--as LABEL] [--format]
func runCardUpdateStatus(args []string) int {
	fs := flag.NewFlagSet("card-update-status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	status := fs.String("status", "", "new status (required): "+
		"todo|in_progress|in_review|done|cancelled")
	as := fs.String("as", "", "optional author label for the auto-recorded status_change event")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 || *status == "" {
		fmt.Fprintln(os.Stderr, "card-update-status: --card and --status required")
		return exitUsage
	}
	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-update-status: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	card, err := st.UpdateCardStatus(ctx, *id, *status, *as)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		fmt.Fprintf(os.Stderr, "card-update-status: not found: %d\n", *id)
		return exitNotFound
	}
	if errors.Is(err, sqlitestore.ErrCardInvalidStatus) {
		fmt.Fprintf(os.Stderr, "card-update-status: %v\n", err)
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-update-status: %v\n", err)
		return exitInternal
	}
	return emitCard(card, *format)
}

// runCardUpdate — card-update --card N [--title T] [--body B]
// [--needs-role R] [--priority P] [--tags JSON] [--context-refs JSON]
// [--as LABEL] [--format json|plain]
//
// Mutates non-status fields. Status, claim ownership, and dependency
// edges have their own dedicated verbs. At least one mutable flag must
// be set or the call is a no-op (returns the card unchanged).
//
// Sentinel values for explicit clear:
//   --body=""             clears the body
//   --needs-role=""       removes role gating
//   --tags="[]"           empties the tags array
func runCardUpdate(args []string) int {
	fs := flag.NewFlagSet("card-update", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	as := fs.String("as", "", "optional author label for the auto-recorded body_update event")
	format := fs.String("format", "plain", "plain|json")

	// Wrap each mutable field with a "set?" boolean so empty-string can
	// be distinguished from absent. flag's standard String() doesn't
	// expose visited; we use fs.Visit after parse instead.
	title := fs.String("title", "", "new title")
	body := fs.String("body", "", "new body / spec (markdown)")
	needsRole := fs.String("needs-role", "", "new needs_role (use \"\" to clear)")
	priority := fs.Int("priority", 0, "new priority (-1 low, 0 normal, 1 high)")
	tags := fs.String("tags", "", "new tags JSON array")
	ctxRefs := fs.String("context-refs", "", "new context_refs JSON object")
	kind := fs.String("kind", "", "promote/demote kind: 'task' or 'epic'")
	splittable := fs.Bool("splittable", false, "set splittable flag (use --splittable=false to clear)")
	trackHandoffs := fs.Bool("track-handoffs", false, "set track_handoffs flag (use --track-handoffs=false to clear)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 {
		fmt.Fprintln(os.Stderr, "card-update: --card required")
		return exitUsage
	}
	if *format != "plain" && *format != "json" {
		fmt.Fprintln(os.Stderr, "card-update: --format must be plain or json")
		return exitUsage
	}
	if *tags != "" && !isValidJSON(*tags) {
		fmt.Fprintln(os.Stderr, "card-update: --tags must be valid JSON")
		return exitUsage
	}
	if *ctxRefs != "" && !isValidJSON(*ctxRefs) {
		fmt.Fprintln(os.Stderr, "card-update: --context-refs must be valid JSON")
		return exitUsage
	}

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	params := sqlitestore.UpdateCardFieldsParams{}
	if visited["title"] {
		params.Title = title
	}
	if visited["body"] {
		params.Body = body
	}
	if visited["needs-role"] {
		params.NeedsRole = needsRole
	}
	if visited["priority"] {
		params.Priority = priority
	}
	if visited["tags"] {
		params.Tags = tags
	}
	if visited["context-refs"] {
		params.ContextRefs = ctxRefs
	}
	if visited["kind"] {
		if *kind != sqlitestore.CardKindTask && *kind != sqlitestore.CardKindEpic {
			fmt.Fprintln(os.Stderr, "card-update: --kind must be 'task' or 'epic'")
			return exitUsage
		}
		params.Kind = kind
	}
	if visited["splittable"] {
		params.Splittable = splittable
	}
	if visited["track-handoffs"] {
		params.TrackHandoffs = trackHandoffs
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-update: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	card, err := st.UpdateCardFields(ctx, *id, params, *as)
	if errors.Is(err, sqlitestore.ErrCardNotFound) {
		fmt.Fprintf(os.Stderr, "card-update: not found: %d\n", *id)
		return exitNotFound
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-update: %v\n", err)
		return exitInternal
	}
	return emitCard(card, *format)
}

// runCardComment — card-comment --card N --body T --as LABEL [--format]
//
// Posts a free-form markdown comment to a card's timeline. Workers use
// this mid-run to narrate progress; humans use it via the drawer
// composer. Auto-recorded events (status_change, claim, body_update,
// run_dispatch, run_complete) live in the same table but are inserted
// by the corresponding mutators — comments are the only kind that
// callers create directly.
func runCardComment(args []string) int {
	fs := flag.NewFlagSet("card-comment", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.Int64("id", 0, "card id (alias of --card)")
	cardID := fs.Int64("card", 0, "card id (required)")
	body := fs.String("body", "", "comment body (required)")
	as := fs.String("as", "", "author label (required)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cardID != 0 {
		*id = *cardID
	}
	if *id == 0 || *body == "" || *as == "" {
		fmt.Fprintln(os.Stderr, "card-comment: --card, --body, --as required")
		return exitUsage
	}

	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-comment: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	ev, err := st.AppendCardEvent(ctx, sqlitestore.AppendCardEventParams{
		CardID: *id, Kind: sqlitestore.CardEventComment,
		Author: *as, Body: *body,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-comment: %v\n", err)
		return exitInternal
	}

	if *format == "json" {
		return emitJSON(map[string]any{
			"id": ev.ID, "card_id": ev.CardID, "kind": ev.Kind,
			"author": ev.Author, "body": ev.Body,
			"created_at": ev.CreatedAt,
		})
	}
	fmt.Printf("event #%d on card #%d by %s: %s\n", ev.ID, ev.CardID, ev.Author, ev.Body)
	return exitOK
}

// runCardAddDep — card-add-dep --blocker N --blockee M --as LABEL [--format]
func runCardAddDep(args []string) int {
	fs := flag.NewFlagSet("card-add-dep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	blocker := fs.Int64("blocker", 0, "blocker card id (required)")
	blockee := fs.Int64("blockee", 0, "blockee card id (required)")
	as := fs.String("as", "", "label of the caller (required, for audit)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *blocker == 0 || *blockee == 0 || *as == "" {
		fmt.Fprintln(os.Stderr, "card-add-dep: --blocker, --blockee, --as required")
		return exitUsage
	}
	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-add-dep: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	if err := st.AddCardDependency(ctx, *blocker, *blockee, *as); err != nil {
		if errors.Is(err, sqlitestore.ErrCardDependencyCycle) ||
			errors.Is(err, sqlitestore.ErrCardDependencySelf) {
			fmt.Fprintf(os.Stderr, "card-add-dep: %v\n", err)
			return exitDataErr
		}
		if errors.Is(err, sqlitestore.ErrCardNotFound) {
			fmt.Fprintf(os.Stderr, "card-add-dep: %v\n", err)
			return exitNotFound
		}
		fmt.Fprintf(os.Stderr, "card-add-dep: %v\n", err)
		return exitInternal
	}
	return emitDepResult(*blocker, *blockee, true, *format)
}

// runCardRemoveDep — card-remove-dep --blocker N --blockee M [--format]
func runCardRemoveDep(args []string) int {
	fs := flag.NewFlagSet("card-remove-dep", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	blocker := fs.Int64("blocker", 0, "blocker card id (required)")
	blockee := fs.Int64("blockee", 0, "blockee card id (required)")
	format := fs.String("format", "plain", "plain|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *blocker == 0 || *blockee == 0 {
		fmt.Fprintln(os.Stderr, "card-remove-dep: --blocker, --blockee required")
		return exitUsage
	}
	ctx, cancel := cardCtx()
	defer cancel()
	st, err := sqlitestore.Open(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-remove-dep: open: %v\n", err)
		return exitInternal
	}
	defer st.Close()

	removed, err := st.RemoveCardDependency(ctx, *blocker, *blockee)
	if err != nil {
		fmt.Fprintf(os.Stderr, "card-remove-dep: %v\n", err)
		return exitInternal
	}
	return emitDepResult(*blocker, *blockee, removed, *format)
}

// --- output helpers ---------------------------------------------------------

func emitCard(c *sqlitestore.Card, format string) int {
	if format == "json" {
		return emitJSON(cardAsMap(c))
	}
	fmt.Printf("%s\n", renderCardPlain(c))
	return exitOK
}

func emitCards(cards []*sqlitestore.Card, format string) int {
	if format == "json" {
		arr := make([]map[string]any, 0, len(cards))
		for _, c := range cards {
			arr = append(arr, cardAsMap(c))
		}
		return emitJSON(arr)
	}
	if len(cards) == 0 {
		fmt.Println("(no cards)")
		return exitOK
	}
	for _, c := range cards {
		fmt.Println(renderCardPlain(c))
	}
	return exitOK
}

func emitDepResult(blocker, blockee int64, ok bool, format string) int {
	if format == "json" {
		return emitJSON(map[string]any{
			"blocker_id": blocker, "blockee_id": blockee, "ok": ok,
		})
	}
	if ok {
		fmt.Printf("ok: %d → %d\n", blocker, blockee)
	} else {
		fmt.Printf("no-op: %d → %d (edge didn't exist)\n", blocker, blockee)
	}
	return exitOK
}

func cardAsMap(c *sqlitestore.Card) map[string]any {
	m := map[string]any{
		"id":         c.ID,
		"pair_key":   c.PairKey,
		"home_host":  c.HomeHost,
		"title":      c.Title,
		"status":     c.Status,
		"created_by": c.CreatedBy,
		"priority":   c.Priority,
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
		"blocker_ids": c.BlockerIDs,
		"blockee_ids": c.BlockeeIDs,
		"ready":       c.Ready,
	}
	if c.Body != "" {
		m["body"] = c.Body
	}
	if c.NeedsRole != "" {
		m["needs_role"] = c.NeedsRole
	}
	if c.ClaimedBy != "" {
		m["claimed_by"] = c.ClaimedBy
	}
	if c.Tags != "" {
		m["tags"] = json.RawMessage(c.Tags)
	}
	if c.ContextRefs != "" {
		m["context_refs"] = json.RawMessage(c.ContextRefs)
	}
	if c.ClaimedAt != "" {
		m["claimed_at"] = c.ClaimedAt
	}
	if c.CompletedAt != "" {
		m["completed_at"] = c.CompletedAt
	}
	if c.AssignedToAgentID != 0 {
		m["assigned_to_agent_id"] = c.AssignedToAgentID
	}
	// v3.12.4.5/.6 — surface decomposition + handoff flags. Kind is
	// always present (defaults 'task'); the bools only when true so the
	// vast majority of cards keep clean output.
	m["kind"] = c.Kind
	if c.Splittable {
		m["splittable"] = true
	}
	if c.TrackHandoffs {
		m["track_handoffs"] = true
	}
	return m
}

func renderCardPlain(c *sqlitestore.Card) string {
	line := "#" + strconv.FormatInt(c.ID, 10) + " [" + c.Status + "]"
	if c.Ready {
		line += " ready"
	} else if c.Status == sqlitestore.CardStatusTodo && len(c.BlockerIDs) > 0 {
		line += fmt.Sprintf(" blocked-by=%v", c.BlockerIDs)
	}
	if c.NeedsRole != "" {
		line += " needs=" + c.NeedsRole
	}
	if c.ClaimedBy != "" {
		line += " claimed=" + c.ClaimedBy
	}
	if c.Priority != 0 {
		line += fmt.Sprintf(" p=%d", c.Priority)
	}
	line += "  " + c.Title
	return line
}

func emitJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "emit-json: %v\n", err)
		return exitInternal
	}
	return exitOK
}

func isValidJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}
