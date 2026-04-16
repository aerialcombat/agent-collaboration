#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP_ROOT="$(mktemp -d)"
TEST_HOME="$TMP_ROOT/home"
TEST_BIN="$TEST_HOME/bin"
SAMPLE_REPO="$TMP_ROOT/sample-repo"
PYTHON_ONLY_BIN="$TMP_ROOT/python-bin"

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

assert_glob_exists() {
  local pattern="$1"
  local matches=()

  shopt -s nullglob
  matches=($pattern)
  shopt -u nullglob

  (( ${#matches[@]} > 0 )) || fail "expected at least one match for glob: $pattern"
}

trap cleanup EXIT

mkdir -p "$TEST_HOME/.claude" "$TEST_HOME/.codex" "$TEST_HOME/.gemini" "$TEST_BIN" "$SAMPLE_REPO/docs"
SAMPLE_REPO_REAL="$(cd "$SAMPLE_REPO" && pwd -P)"
mkdir -p "$PYTHON_ONLY_BIN"
ln -s "$(command -v python3)" "$PYTHON_ONLY_BIN/python3"
for tool in bash basename cat date dirname grep head mkdir mktemp mv rm sleep tr wc; do
  ln -s "$(command -v "$tool")" "$PYTHON_ONLY_BIN/$tool"
done

cat > "$TEST_HOME/.claude/CLAUDE.md" <<'EOF'
# Existing Claude Defaults
keep this content
EOF

cat > "$TEST_HOME/.codex/AGENTS.md" <<'EOF'
# Existing Codex Defaults
keep this content
EOF

cat > "$TEST_HOME/.gemini/GEMINI.md" <<'EOF'
# Existing Gemini Defaults
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
assert_glob_exists "$TEST_HOME/.claude/CLAUDE.md.bak.*"
assert_glob_exists "$TEST_HOME/.codex/AGENTS.md.bak.*"
assert_file "$TEST_HOME/.gemini/GEMINI.md"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "# Existing Gemini Defaults"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "<!-- BEGIN agent-collaboration -->"
assert_glob_exists "$TEST_HOME/.gemini/GEMINI.md.bak.*"

"$PROJECT_ROOT/scripts/doctor-global-protocol" >/dev/null

cat > "$SAMPLE_REPO/AGENT-COLLABORATION.md" <<'EOF'
# Agent Collaboration

Sample repo guide.
EOF

cat > "$SAMPLE_REPO/.agent-collab.env" <<'EOF'
AGENT_COLLAB_GUIDE=AGENT-COLLABORATION.md
AGENT_COLLAB_REVIEW_DIR=.agent-collab/reviews
AGENT_COLLAB_DEFAULT_CHALLENGER=claude
AGENT_COLLAB_TIMEOUT_SECONDS=15
AGENT_COLLAB_CLAUDE_TIMEOUT_SECONDS=12
AGENT_COLLAB_CODEX_TIMEOUT_SECONDS=9
AGENT_COLLAB_GEMINI_TIMEOUT_SECONDS=9
AGENT_COLLAB_CLAUDE_EFFORT=low
EOF

cat > "$SAMPLE_REPO/docs/scope.md" <<'EOF'
# Scope

Sample scope.
EOF

python3 - <<'EOF' > "$SAMPLE_REPO/docs/large-context.txt"
print("large context line")
for _ in range(3500):
  print("0123456789abcdef" * 4)
EOF

dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run." --dry-run)"
[[ "$dry_run_output" == *"challenger: claude"* ]] || fail "dry run did not use default challenger"
[[ "$dry_run_output" == *"repo_root: $SAMPLE_REPO_REAL"* ]] || fail "dry run did not detect sample repo root"
[[ "$dry_run_output" == *"Repo-local collaboration guide: AGENT-COLLABORATION.md"* ]] || fail "dry run did not include repo guide"
[[ "$dry_run_output" == *"timeout_seconds: 12"* ]] || fail "dry run did not include claude timeout override"

guide_scope_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" verify --scope AGENT-COLLABORATION.md --dry-run)"
[[ "$(grep -c '^----- BEGIN FILE: AGENT-COLLABORATION.md -----$' <<<"$guide_scope_dry_run_output")" == "1" ]] || fail "dry run should inline guide/scope overlap only once"

guide_context_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" verify --context AGENT-COLLABORATION.md --dry-run)"
[[ "$(grep -c '^----- BEGIN FILE: AGENT-COLLABORATION.md -----$' <<<"$guide_context_dry_run_output")" == "1" ]] || fail "dry run should inline guide/context overlap only once"

codex_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Dry run codex." --dry-run)"
[[ "$codex_dry_run_output" == *"timeout_seconds: 9"* ]] || fail "dry run did not include codex timeout override"

cli_override_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run override." --timeout-seconds 3 --dry-run)"
[[ "$cli_override_dry_run_output" == *"timeout_seconds: 3"* ]] || fail "dry run did not honor timeout CLI override"

disabled_timeout_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --scope docs/scope.md --prompt "Dry run disabled timeout." --timeout-seconds 0 --dry-run)"
[[ "$disabled_timeout_dry_run_output" == *"timeout_seconds: 0"* ]] || fail "dry run did not allow timeout disable"
[[ "$disabled_timeout_dry_run_output" == *"timeout_backend: disabled"* ]] || fail "dry run did not report disabled timeout backend"

gemini_dry_run_output="$(cd "$SAMPLE_REPO" && "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Dry run gemini." --dry-run)"
[[ "$gemini_dry_run_output" == *"timeout_seconds: 9"* ]] || fail "dry run did not include gemini timeout override"

mkdir -p "$TMP_ROOT/mock-bin"
cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Review the provided file contents directly."* ]] || exit 1
[[ "$stdin_payload" == *"----- BEGIN FILE: AGENT-COLLABORATION.md -----"* ]] || exit 1
[[ "$stdin_payload" == *"Sample repo guide."* ]] || exit 1
[[ "$stdin_payload" == *"----- BEGIN FILE: docs/scope.md -----"* ]] || exit 1
[[ "$stdin_payload" == *"Sample scope."* ]] || exit 1

expect_tools_arg=0
saw_tools_arg=0
saw_effort_flag=0
saw_low_effort=0
for arg in "$@"; do
  if (( expect_tools_arg )); then
    [[ "$arg" == "Read" ]] || exit 1
    saw_tools_arg=1
    expect_tools_arg=0
    continue
  fi

  if [[ "$arg" == "--tools" ]]; then
    expect_tools_arg=1
    continue
  fi

  if [[ "$arg" == "--effort" ]]; then
    saw_effort_flag=1
    continue
  fi

  if (( saw_effort_flag )); then
    [[ "$arg" == "low" ]] || exit 1
    saw_low_effort=1
    saw_effort_flag=0
  fi
done

(( ! saw_tools_arg )) || exit 1
(( saw_low_effort )) || exit 1
printf 'mock claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

claude_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Inline prompt check."
)"
[[ "$claude_run_output" == review:\ * ]] || fail "claude challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"----- BEGIN FILE: docs/large-context.txt -----"* ]] || exit 1
[[ "$stdin_payload" == *"[truncated: showing first"* ]] || exit 1
[[ "$stdin_payload" == *"Use the Read tool only if you need missing content."* ]] || exit 1

expect_tools_arg=0
saw_tools_arg=0
for arg in "$@"; do
  if (( expect_tools_arg )); then
    [[ "$arg" == "Read" ]] || exit 1
    saw_tools_arg=1
    expect_tools_arg=0
    continue
  fi

  if [[ "$arg" == "--tools" ]]; then
    expect_tools_arg=1
  fi
done

(( saw_tools_arg )) || exit 1
printf 'truncated claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

truncated_claude_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" verify --challenger claude --scope docs/scope.md --context docs/large-context.txt --prompt "Truncation check."
)"
[[ "$truncated_claude_run_output" == review:\ * ]] || fail "claude verify run did not report review output for truncated context"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Python fallback check."* ]] || exit 1
sleep 1
printf 'python fallback claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

python_fallback_dry_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Python fallback dry run." --dry-run
)"
[[ "$python_fallback_dry_run_output" == *"timeout_backend: python3"* ]] || fail "claude python fallback dry run did not force python3 backend"

python_fallback_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Python fallback check." --timeout-seconds 2
)"
[[ "$python_fallback_output" == review:\ * ]] || fail "claude challenger run did not succeed with python3 timeout fallback"

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

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
sleep 5
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

if claude_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude timeout test should fail"
fi
[[ "$claude_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude timeout failure did not report timeout"

cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
trap '' TERM
cat >/dev/null
sleep 20
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

SECONDS=0
if claude_hard_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude hard timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude hard-timeout test should fail"
fi
claude_hard_timeout_elapsed="$SECONDS"
[[ "$claude_hard_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude hard-timeout failure did not report timeout"
(( claude_hard_timeout_elapsed < 10 )) || fail "claude hard-timeout path took too long: ${claude_hard_timeout_elapsed}s"

SECONDS=0
if claude_python_hard_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Claude python hard timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "claude python hard-timeout test should fail"
fi
claude_python_hard_timeout_elapsed="$SECONDS"
[[ "$claude_python_hard_timeout_output" == *"claude review timed out after 1s"* ]] || fail "claude python hard-timeout failure did not report timeout"
(( claude_python_hard_timeout_elapsed < 10 )) || fail "claude python hard-timeout path took too long: ${claude_python_hard_timeout_elapsed}s"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
last=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
  last="$arg"
done

[[ -n "$out" ]] || exit 1
[[ "$last" == "-" ]] || exit 1

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Codex stdin check."* ]] || exit 1

printf 'mock codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

codex_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex stdin check."
)"
[[ "$codex_run_output" == review:\ * ]] || fail "codex challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
done

cat >/dev/null
sleep 1
printf 'python fallback codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

codex_python_fallback_dry_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python fallback dry run." --dry-run
)"
[[ "$codex_python_fallback_dry_run_output" == *"timeout_backend: python3"* ]] || fail "codex python fallback dry run did not force python3 backend"

codex_python_success_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python success check." --timeout-seconds 2
)"
[[ "$codex_python_success_output" == review:\ * ]] || fail "codex challenger run did not succeed with python3 timeout fallback"

cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
done

trap '' TERM
cat >/dev/null
sleep 20
printf 'late output\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

SECONDS=0
if codex_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "codex timeout test should fail"
fi
codex_timeout_elapsed="$SECONDS"
[[ "$codex_timeout_output" == *"codex review timed out after 1s"* ]] || fail "codex timeout failure did not report timeout"
(( codex_timeout_elapsed < 10 )) || fail "codex hard-timeout path took too long: ${codex_timeout_elapsed}s"

SECONDS=0
if codex_python_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Codex python timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "codex python timeout test should fail"
fi
codex_python_timeout_elapsed="$SECONDS"
[[ "$codex_python_timeout_output" == *"codex review timed out after 1s"* ]] || fail "codex python timeout failure did not report timeout"
(( codex_python_timeout_elapsed < 10 )) || fail "codex python hard-timeout path took too long: ${codex_python_timeout_elapsed}s"

# Direction: Claude/Codex -> Gemini (gemini as challenger)
cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini stdin check."* ]] || exit 1

printf 'mock gemini review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

gemini_run_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini stdin check."
)"
[[ "$gemini_run_output" == review:\ * ]] || fail "gemini challenger run did not report review output"

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
trap '' TERM
cat >/dev/null
sleep 20
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

SECONDS=0
if gemini_timeout_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini timeout check." --timeout-seconds 1 2>&1
)"; then
  fail "gemini timeout test should fail"
fi
gemini_timeout_elapsed="$SECONDS"
[[ "$gemini_timeout_output" == *"gemini review timed out after 1s"* ]] || fail "gemini timeout failure did not report timeout"
(( gemini_timeout_elapsed < 10 )) || fail "gemini hard-timeout path took too long: ${gemini_timeout_elapsed}s"

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
printf '\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

if (
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini whitespace failure check."
); then
  fail "gemini whitespace-only challenger output should fail"
fi

cat > "$TMP_ROOT/mock-bin/gemini" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
cat >/dev/null
sleep 1
printf 'python fallback gemini review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/gemini"

gemini_python_fallback_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$PYTHON_ONLY_BIN:$TEST_BIN" \
    "$TEST_BIN/agent-collab" challenge --challenger gemini --scope docs/scope.md --prompt "Gemini python fallback check." --timeout-seconds 2
)"
[[ "$gemini_python_fallback_output" == review:\ * ]] || fail "gemini challenger run did not succeed with python3 timeout fallback"

# Direction: Gemini -> Claude (claude as challenger, from gemini lead perspective)
cat > "$TMP_ROOT/mock-bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini-led claude review."* ]] || exit 1

saw_effort_flag=0
saw_low_effort=0
for arg in "$@"; do
  if [[ "$arg" == "--effort" ]]; then
    saw_effort_flag=1
    continue
  fi
  if (( saw_effort_flag )); then
    [[ "$arg" == "low" ]] || exit 1
    saw_low_effort=1
    saw_effort_flag=0
  fi
done
(( saw_low_effort )) || exit 1

printf 'gemini-led claude review\n'
EOF
chmod +x "$TMP_ROOT/mock-bin/claude"

gemini_to_claude_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger claude --scope docs/scope.md --prompt "Gemini-led claude review."
)"
[[ "$gemini_to_claude_output" == review:\ * ]] || fail "gemini->claude direction did not report review output"

# Direction: Gemini -> Codex (codex as challenger, from gemini lead perspective)
cat > "$TMP_ROOT/mock-bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

out=""
last=""
previous=""

for arg in "$@"; do
  if [[ "$previous" == "-o" || "$previous" == "--output-last-message" ]]; then
    out="$arg"
  fi
  previous="$arg"
  last="$arg"
done

[[ -n "$out" ]] || exit 1
[[ "$last" == "-" ]] || exit 1

stdin_payload="$(cat)"
[[ "$stdin_payload" == *"Gemini-led codex review."* ]] || exit 1

printf 'gemini-led codex review\n' > "$out"
EOF
chmod +x "$TMP_ROOT/mock-bin/codex"

gemini_to_codex_output="$(
  cd "$SAMPLE_REPO" &&
    PATH="$TMP_ROOT/mock-bin:$TEST_BIN:$PATH" \
    "$TEST_BIN/agent-collab" challenge --challenger codex --scope docs/scope.md --prompt "Gemini-led codex review."
)"
[[ "$gemini_to_codex_output" == review:\ * ]] || fail "gemini->codex direction did not report review output"

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
assert_file "$TEST_HOME/.gemini/GEMINI.md"
assert_contains "$TEST_HOME/.gemini/GEMINI.md" "# Existing Gemini Defaults"
if grep -Fq "<!-- BEGIN agent-collaboration -->" "$TEST_HOME/.gemini/GEMINI.md"; then
  fail "managed block still present in Gemini file after uninstall"
fi
[[ ! -e "$TEST_BIN/agent-collab" ]] || fail "agent-collab command still present after uninstall"
assert_glob_exists "$TEST_BIN/agent-collab.bak.*"

echo "PASS: smoke"
