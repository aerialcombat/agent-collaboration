package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
)

// GetSessionKeyForSelf reads the session_key column for a given
// (cwd, label). Returns "" when the row is missing or the column is
// NULL. Used by session-register's codex/gemini idempotency path.
func (s *SQLiteLocal) GetSessionKeyForSelf(ctx context.Context, cwd, label string) (string, error) {
	var sk sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT session_key FROM sessions WHERE cwd = ? AND label = ?`,
		cwd, label,
	).Scan(&sk)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", fmt.Errorf("get session_key: %w", err)
	}
	if !sk.Valid {
		return "", nil
	}
	return sk.String, nil
}

// Label / pair-key / host validators. Byte-for-byte parity with
// scripts/peer-inbox-db.py's LABEL_RE / PAIR_KEY_RE / HOST_LABEL_RE
// so the same rejection messages surface from both impls.

var (
	labelRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	pairKeyRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)
	hostLblRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hostSubstR = regexp.MustCompile(`[^a-z0-9-]`)
)

var validAgents = map[string]struct{}{
	"claude": {}, "codex": {}, "gemini": {}, "pi": {}, "human": {},
}

// ValidateLabel returns nil if label matches LABEL_RE, else an error
// whose string is byte-identical to Python's err() message.
func ValidateLabel(label string) error {
	if !labelRE.MatchString(label) {
		return fmt.Errorf("invalid label '%s' (allowed: [a-z0-9][a-z0-9_-]{0,63})", label)
	}
	return nil
}

// ValidatePairKey mirrors Python's validate_pair_key.
func ValidatePairKey(k string) error {
	if !pairKeyRE.MatchString(k) {
		return fmt.Errorf("invalid pair key '%s' (allowed: [a-z0-9][a-z0-9-]{1,63})", k)
	}
	return nil
}

// ValidateHostLabel mirrors Python's validate_host_label.
func ValidateHostLabel(h string) error {
	if !hostLblRE.MatchString(h) {
		return fmt.Errorf("invalid host label '%s' (allowed: [a-z0-9][a-z0-9-]{0,63})", h)
	}
	return nil
}

// ValidateAgent mirrors Python's validate_agent.
func ValidateAgent(a string) error {
	if _, ok := validAgents[a]; !ok {
		return fmt.Errorf("invalid agent '%s' (allowed: [claude codex gemini human pi])", a)
	}
	return nil
}

// SelfHost mirrors Python's self_host: explicit AGENT_COLLAB_SELF_HOST
// env wins, else sanitized os.Hostname lowercased, else "localhost".
func SelfHost() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_COLLAB_SELF_HOST"))); v != "" {
		if ValidateHostLabel(v) == nil {
			return v
		}
	}
	raw, err := os.Hostname()
	if err != nil || raw == "" {
		return "localhost"
	}
	sanitized := strings.Trim(hostSubstR.ReplaceAllString(strings.ToLower(raw), "-"), "-")
	if sanitized == "" || !hostLblRE.MatchString(sanitized) {
		return "localhost"
	}
	return sanitized
}

// generatePairKey mirrors Python's adjective-noun-xxxx shape. Uses
// crypto/rand for the hex suffix so tokens aren't predictable.
// The wordlist is duplicated from peer-inbox-db.py (PAIR_KEY_ADJECTIVES
// / PAIR_KEY_NOUNS); any divergence in the Python lists should be
// mirrored here or the slug space drifts between impls.
func generatePairKey() string {
	adj := pairKeyAdjectives[randIndex(len(pairKeyAdjectives))]
	noun := pairKeyNouns[randIndex(len(pairKeyNouns))]
	suf := make([]byte, 2)
	_, _ = rand.Read(suf)
	return fmt.Sprintf("%s-%s-%s", adj, noun, hex.EncodeToString(suf))
}

// NewAutoLabel returns an adjective-noun slug, matching Python's
// generate_label. Exposed for the CLI's session-register autogen path.
func NewAutoLabel() string {
	return fmt.Sprintf("%s-%s",
		pairKeyAdjectives[randIndex(len(pairKeyAdjectives))],
		pairKeyNouns[randIndex(len(pairKeyNouns))],
	)
}

func randIndex(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

var pairKeyAdjectives = []string{
	"amber", "ancient", "arctic", "azure", "bold", "brave", "brisk", "bright",
	"bronze", "busy", "calm", "candid", "chilly", "chipper", "clear", "clever",
	"cosmic", "crimson", "crisp", "curious", "dainty", "daring", "dashing",
	"deft", "dewy", "dusty", "eager", "earnest", "easy", "ebon", "eerie",
	"elated", "electric", "emerald", "epic", "fancy", "fearless", "fertile",
	"fierce", "fleet", "floral", "fluffy", "fond", "fresh", "frosty", "furry",
	"gentle", "glad", "gleaming", "glowing", "golden", "gracious", "grand",
	"happy", "hardy", "hazy", "hearty", "honest", "humble", "idle", "inky",
	"jade", "jazzy", "jolly", "jovial", "keen", "kind", "lavender", "lean",
	"lively", "lucky", "lush", "magnetic", "merry", "mindful", "misty",
	"modest", "noble", "nimble", "ochre", "peaceful", "perky", "placid",
	"playful", "plucky", "polite", "proud", "quick", "quiet", "radiant",
	"rapid", "rosy", "royal", "rugged", "sage", "serene", "silent", "silky",
	"silver", "snowy", "soft", "solar", "solemn", "sparkly", "spry", "stellar",
	"sturdy", "sunny", "swift", "tangy", "tender", "tidy", "tranquil",
	"trusty", "upbeat", "urban", "valiant", "velvet", "vibrant", "vivid",
	"warm", "wild", "windy", "witty", "woodland", "zany", "zealous", "zesty",
}

var pairKeyNouns = []string{
	"acorn", "anchor", "archer", "arrow", "aspen", "badger", "basalt", "beacon",
	"beaver", "birch", "bison", "bluff", "boulder", "breeze", "brook", "canyon",
	"cedar", "chestnut", "cliff", "clover", "comet", "compass", "coral",
	"cove", "creek", "crest", "crystal", "cypress", "daisy", "delta", "dove",
	"eagle", "ember", "fable", "falcon", "feather", "fern", "fjord", "flame",
	"forest", "fox", "galaxy", "glade", "glow", "goose", "grove", "gull",
	"harbor", "harvest", "haven", "hazel", "heath", "hedge", "heron", "hill",
	"holly", "horizon", "island", "ivy", "jaguar", "juniper", "kestrel",
	"lagoon", "lake", "lantern", "lark", "laurel", "lilac", "lion", "lotus",
	"lynx", "maple", "meadow", "mesa", "monsoon", "moon", "moss", "mountain",
	"nebula", "oak", "oasis", "orchid", "otter", "owl", "palm", "peak",
	"peony", "pine", "plum", "poppy", "prairie", "quartz", "quail", "rainbow",
	"ranger", "raven", "reef", "ridge", "river", "robin", "sable", "saffron",
	"salmon", "sequoia", "shore", "slate", "sparrow", "spring", "spruce",
	"star", "storm", "stream", "summit", "swallow", "thicket", "thistle",
	"tide", "tiger", "trail", "tulip", "valley", "vista", "willow", "wolf",
	"wren", "zebra", "zenith", "zephyr",
}
