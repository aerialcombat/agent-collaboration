#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP_ROOT="$(mktemp -d)"
TEST_HOME="$TMP_ROOT/home"
TEST_BIN="$TEST_HOME/bin"
SAMPLE_REPO="$TMP_ROOT/sample-repo"

cleanup() {
  rm -rf "$TMP_ROOT"
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_file() {
  local path="$1"
  [[ -f "$path" ]] || fail "missing file: $path"
}

assert_executable() {
  local path="$1"
  [[ -x "$path" ]] || fail "missing executable: $path"
}

assert_contains() {
  local path="$1"
  local pattern="$2"
  grep -Fq "$pattern" "$path" || fail "expected '$pattern' in $path"
}

trap cleanup EXIT

mkdir -p "$TEST_HOME/.claude" "$TEST_HOME/.codex" "$TEST_BIN" "$SAMPLE_REPO/docs"
SAMPLE_REPO_REAL="$(cd "$SAMPLE_REPO" && pwd -P)"

cat > "$TEST_HOME/.claude/CLAUDE.md" <<'EOF'
# Existing Claude Defaults
keep this content
EOF

cat > "$TEST_HOME/.codex/AGENTS.md" <<'EOF'
# Existing Codex Defaults
keep this content
EOF

export HOME="$TEST_HOME"
export PATH="$TEST_BIN:$PATH"
export AGENT_COLLAB_BIN_DIR="$TEST_BIN"

"$PROJECT_ROOT/scripts/install-global-protocol" >/dev/null

assert_file "$TEST_HOME/.claude/CLAUDE.md"
assert_file "$TEST_HOME/.codex/AGENTS.md"
assert_executable "$TEST_BIN/agent-collab"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "# Existing Claude Defaults"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "<!-- BEGIN agent-collaboration -->"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "# Existing Codex Defaults"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "<!-- BEGIN agent-collaboration -->"

"$PROJECT_ROOT/scripts/doctor-global-protocol" >/dev/null

cat > "$SAMPLE_REPO/AGENT-COLLABORATION.md" <<'EOF'
# Agent Collaboration

Sample repo guide.
EOF

cat > "$SAMPLE_REPO/.agent-collab.env" <<'EOF'
AGENT_COLLAB_GUIDE=AGENT-COLLABORATION.md
AGENT_COLLAB_REVIEW_DIR=.agent-collab/reviews
AGENT_COLLAB_DEFAULT_CHALLENGER=claude
EOF

cat > "$SAMPLE_REPO/docs/scope.md" <<'EOF'
# Scope

Sample scope.
EOF

dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run." --dry-run)"
[[ "$dry_run_output" == *"challenger: claude"* ]] || fail "dry run did not use default challenger"
[[ "$dry_run_output" == *"repo_root: $SAMPLE_REPO_REAL"* ]] || fail "dry run did not detect sample repo root"
[[ "$dry_run_output" == *"Repo-local collaboration guide: AGENT-COLLABORATION.md"* ]] || fail "dry run did not include repo guide"

mkdir -p "$TMP_ROOT/mock-bin"
cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
printf '\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

if (
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Whitespace failure check."
); then
  fail "whitespace-only challenger output should fail"
fi

"$PROJECT_ROOT/scripts/install-global-protocol" --uninstall >/dev/null

assert_file "$TEST_HOME/.claude/CLAUDE.md"
assert_file "$TEST_HOME/.codex/AGENTS.md"
assert_contains "$TEST_HOME/.claude/CLAUDE.md" "# Existing Claude Defaults"
assert_contains "$TEST_HOME/.codex/AGENTS.md" "# Existing Codex Defaults"
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.claude/CLAUDE.md"; then
  fail "managed block still present in Claude file after uninstall"
fi
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.codex/AGENTS.md"; then
  fail "managed block still present in Codex file after uninstall"
fi
[[ ! -e "$TEST_BIN/agent-collab" ]] || fail "agent-collab command still present after uninstall"

echo "PASS: smoke"
