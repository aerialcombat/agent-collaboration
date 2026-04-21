#!/usr/bin/env bash
# server-bootstrap.sh — one-shot installer for agent-collab on a Linux
# server (Ubuntu 22.04+ / Debian 12+). Idempotent; safe to re-run.
#
# Run as root (or with sudo). Assumes the agent-collab repo is already
# cloned locally — this script configures the *machine*, not the repo.
#
# What it does:
#   1. Checks OS + sudo access.
#   2. apt installs runtime deps (python3, nodejs, git, sqlite3, curl).
#   3. Installs Go 1.25+ if not already present.
#   4. Creates the `agent-collab` system user + home dir.
#   5. Builds Go binaries from this repo into go/bin/.
#   6. Symlinks binaries into /usr/local/bin/ so systemd can find them.
#   7. Runs scripts/install-global-protocol as the agent-collab user
#      (installs CLAUDE.md / AGENTS.md / GEMINI.md templates, agent-
#      collab CLI, peer-inbox python, pi extension, hooks).
#   8. Seeds /etc/agent-collab/env.example (no real secrets).
#   9. Installs systemd units from deploy/systemd/.
#  10. Prints next steps — does NOT start services (owner fills env first).
#
# Usage:
#   cd /path/to/agent-collaboration
#   sudo bash scripts/server-bootstrap.sh
#
# Env overrides:
#   AGENT_COLLAB_USER    — system user name (default: agent-collab)
#   AGENT_COLLAB_HOME    — user home dir (default: /var/lib/agent-collab)
#   GO_VERSION           — Go toolchain version to install if missing (default: 1.25.5)

set -euo pipefail

AC_USER="${AGENT_COLLAB_USER:-agent-collab}"
AC_HOME="${AGENT_COLLAB_HOME:-/var/lib/agent-collab}"
GO_VERSION="${GO_VERSION:-1.25.5}"
# GO_SCOPE: "system" installs Go at /usr/local/go (default; simplest).
# "user" installs under $AC_HOME/go and never touches a system-wide Go
# — use this on shared servers where another stack already expects a
# different system Go. Setting this to "skip" assumes Go is already
# present at the required version and refuses to touch it.
GO_SCOPE="${GO_SCOPE:-system}"
# PORT: peer-web bind port. Preflight checks it's free before we build.
PORT="${AGENT_COLLAB_PORT:-8787}"

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SYSTEMD_SRC_DIR="$PROJECT_ROOT/deploy/systemd"
SYSTEMD_TARGET_DIR="/etc/systemd/system"
ENV_SRC="$PROJECT_ROOT/deploy/env.example"
ENV_TARGET_DIR="/etc/agent-collab"
ENV_TARGET_EXAMPLE="$ENV_TARGET_DIR/env.example"
ENV_TARGET_REAL="$ENV_TARGET_DIR/env"
BIN_LINK_DIR="/usr/local/bin"
BINARIES=(agent-collab-daemon peer-web peer-inbox peer-inbox-hook peer-inbox-migrate)

log() { printf '%s\n' "[bootstrap] $*"; }
err() { printf '%s\n' "[bootstrap] ERROR: $*" >&2; exit 1; }

# ---- Preflight ------------------------------------------------------------

[[ "$EUID" -eq 0 ]] || err "must be run as root (try: sudo bash $0)"

if ! command -v apt-get >/dev/null 2>&1; then
  err "only apt-based distros supported today (Ubuntu 22.04+ / Debian 12+)"
fi

[[ -d "$PROJECT_ROOT/go" && -d "$PROJECT_ROOT/scripts" ]] || \
  err "PROJECT_ROOT=$PROJECT_ROOT doesn't look like the agent-collab repo"

# ---- Port preflight -------------------------------------------------------

if command -v ss >/dev/null 2>&1; then
  if ss -tln "( sport = :$PORT )" 2>/dev/null | grep -q ":$PORT"; then
    err "port $PORT is already in use on this host. Set AGENT_COLLAB_PORT to a free port or free :$PORT first. (ss -tlnp | grep :$PORT shows the owner.)"
  fi
elif command -v lsof >/dev/null 2>&1; then
  if lsof -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    err "port $PORT is already in use on this host. Set AGENT_COLLAB_PORT to a free port or free :$PORT first."
  fi
fi

# ---- 1. apt deps ----------------------------------------------------------

log "updating apt + installing runtime deps"
apt-get update -qq
apt-get install -y --no-install-recommends \
  ca-certificates curl git python3 python3-pip \
  sqlite3 \
  build-essential

# Node 20 via NodeSource — Ubuntu's default nodejs is 18, too old for pi
# (uses Node 20+ regex flags in pi-tui). NodeSource adds its apt repo,
# then we install nodejs from there.
if ! command -v node >/dev/null 2>&1 || [[ "$(node --version 2>/dev/null | tr -d 'v' | cut -d. -f1)" -lt 20 ]]; then
  log "installing Node 20 via NodeSource"
  curl -fsSL https://deb.nodesource.com/setup_20.x | bash - >/dev/null 2>&1
  apt-get install -y nodejs >/dev/null 2>&1
  log "  node $(node --version) / npm $(npm --version)"
else
  log "node $(node --version) OK"
fi

# ---- 2. Go toolchain ------------------------------------------------------

# Resolve installation strategy based on GO_SCOPE. "system" writes to
# /usr/local/go — clean on a dedicated host, but risky on a shared box
# where another stack already depends on a specific system Go. "user"
# writes to $AC_HOME/go and is invisible to other users. "skip" assumes
# the already-installed Go is good enough and refuses to touch anything.

GO_BIN=""
need_go_install=0

case "$GO_SCOPE" in
  system)
    if command -v go >/dev/null 2>&1; then
      have_go="$(go version | awk '{print $3}' | sed 's/^go//')"
      if [[ "$(printf '%s\n%s\n' "$GO_VERSION" "$have_go" | sort -V | head -n1)" != "$GO_VERSION" ]]; then
        log "go is $have_go, need >= $GO_VERSION — will install system-wide"
        need_go_install=1
      else
        log "go $have_go OK (>= $GO_VERSION), system scope"
        GO_BIN="$(command -v go)"
      fi
    else
      need_go_install=1
    fi
    ;;
  user)
    user_go="$AC_HOME/go/bin/go"
    if [[ -x "$user_go" ]]; then
      have_go="$("$user_go" version | awk '{print $3}' | sed 's/^go//')"
      if [[ "$(printf '%s\n%s\n' "$GO_VERSION" "$have_go" | sort -V | head -n1)" != "$GO_VERSION" ]]; then
        log "user-scoped go at $user_go is $have_go, need >= $GO_VERSION — will reinstall"
        need_go_install=1
      else
        log "user-scoped go $have_go OK (>= $GO_VERSION)"
        GO_BIN="$user_go"
      fi
    else
      log "no user-scoped go at $user_go — will install"
      need_go_install=1
    fi
    ;;
  skip)
    command -v go >/dev/null 2>&1 || err "GO_SCOPE=skip but no \`go\` found on PATH"
    have_go="$(go version | awk '{print $3}' | sed 's/^go//')"
    [[ "$(printf '%s\n%s\n' "$GO_VERSION" "$have_go" | sort -V | head -n1)" == "$GO_VERSION" ]] || \
      err "GO_SCOPE=skip but existing go is $have_go, need >= $GO_VERSION"
    log "GO_SCOPE=skip — using existing go $have_go at $(command -v go)"
    GO_BIN="$(command -v go)"
    ;;
  *)
    err "unknown GO_SCOPE=$GO_SCOPE (use: system | user | skip)"
    ;;
esac

if [[ "$need_go_install" -eq 1 ]]; then
  arch="$(dpkg --print-architecture)"
  case "$arch" in
    amd64) go_arch=amd64 ;;
    arm64) go_arch=arm64 ;;
    *) err "unsupported arch: $arch (only amd64 / arm64 supported)" ;;
  esac
  tarball="go${GO_VERSION}.linux-${go_arch}.tar.gz"
  log "downloading ${tarball}"
  curl -fsSL -o "/tmp/$tarball" "https://go.dev/dl/$tarball"

  if [[ "$GO_SCOPE" == "user" ]]; then
    # Install under $AC_HOME/go. We need the user's home to exist first
    # — step 3 creates it, but to avoid a step ordering issue we create
    # the minimum dir here and chown later.
    install -d -m 755 -o root -g root "$AC_HOME"
    rm -rf "$AC_HOME/go"
    tar -C "$AC_HOME" -xzf "/tmp/$tarball"
    rm -f "/tmp/$tarball"
    GO_BIN="$AC_HOME/go/bin/go"
    # Chown happens after step 3 (user creation) — marker it.
    USER_GO_INSTALLED=1
  else
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/$tarball"
    rm -f "/tmp/$tarball"
    if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
      echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
      chmod 644 /etc/profile.d/go.sh
    fi
    export PATH="$PATH:/usr/local/go/bin"
    GO_BIN="/usr/local/go/bin/go"
  fi
fi

# ---- 3. agent-collab system user -----------------------------------------

if id "$AC_USER" >/dev/null 2>&1; then
  log "user $AC_USER already exists"
else
  log "creating system user $AC_USER with home $AC_HOME"
  useradd --system --create-home --home-dir "$AC_HOME" --shell /bin/bash "$AC_USER"
fi
# Ensure home perms are sane even if user existed.
chown "$AC_USER:$AC_USER" "$AC_HOME"
chmod 750 "$AC_HOME"
# If user-scoped Go was unpacked earlier (before user existed), hand
# ownership of the unpacked tree to the agent-collab user now.
if [[ "${USER_GO_INSTALLED:-0}" -eq 1 ]]; then
  chown -R "$AC_USER:$AC_USER" "$AC_HOME/go"
fi

# ---- 4. Build Go binaries ------------------------------------------------

log "building Go binaries from $PROJECT_ROOT/go (using $GO_BIN)"
(
  cd "$PROJECT_ROOT/go"
  mkdir -p bin
  # Module cache lives under a stable path so sequential bootstrap
  # runs are fast. Keep it out of the source tree.
  export GOCACHE="${GOCACHE:-/var/cache/agent-collab/gocache}"
  export GOMODCACHE="${GOMODCACHE:-/var/cache/agent-collab/gomodcache}"
  mkdir -p "$GOCACHE" "$GOMODCACHE"
  for sub in daemon hook migrate peer-inbox peer-web; do
    [[ -d "./cmd/$sub" ]] || continue
    out_name="$sub"
    case "$sub" in
      daemon) out_name="agent-collab-daemon" ;;
      hook)   out_name="peer-inbox-hook" ;;
      migrate) out_name="peer-inbox-migrate" ;;
    esac
    log "  go build ./cmd/$sub -> bin/$out_name"
    "$GO_BIN" build -o "bin/$out_name" "./cmd/$sub"
  done
)

# ---- 5. Symlink binaries to /usr/local/bin --------------------------------

log "symlinking binaries into $BIN_LINK_DIR"
for bin in "${BINARIES[@]}"; do
  src="$PROJECT_ROOT/go/bin/$bin"
  dst="$BIN_LINK_DIR/$bin"
  if [[ -x "$src" ]]; then
    ln -sfn "$src" "$dst"
  else
    log "  (skipping $bin — not built)"
  fi
done

# ---- 6. Agent CLIs via npm ------------------------------------------------

# claude-code + pi CLIs (+ pi's ecosystem) installed globally so the
# daemon can spawn them from /usr/local/bin. These are the runtime
# peers the daemon manages — install-global-protocol only configures
# their dotfiles; the binaries have to be present separately.
log "installing agent CLIs globally (claude-code, pi, pi-subagents, pi-zai-glm)"
npm install -g --silent \
  @anthropic-ai/claude-code \
  @mariozechner/pi-coding-agent \
  pi-subagents \
  pi-zai-glm 2>&1 | grep -vE '^npm WARN EBADENGINE|npm WARN deprecated' | tail -5 || true
for cli in claude pi; do
  loc="$(command -v "$cli" 2>/dev/null || true)"
  if [[ -n "$loc" ]]; then
    log "  $cli → $loc"
  else
    log "  warning: $cli not found post-install (check npm global prefix)"
  fi
done

# ---- 7. Per-user install via install-global-protocol ---------------------

log "running install-global-protocol as $AC_USER"
sudo -u "$AC_USER" -H bash -lc "cd $PROJECT_ROOT && ./scripts/install-global-protocol"

# ---- 8. /etc/agent-collab/env template -----------------------------------

install -d -m 750 -o root -g "$AC_USER" "$ENV_TARGET_DIR"
if [[ -f "$ENV_SRC" ]]; then
  install -m 640 -o root -g "$AC_USER" "$ENV_SRC" "$ENV_TARGET_EXAMPLE"
  log "wrote $ENV_TARGET_EXAMPLE"
else
  log "warning: $ENV_SRC not found — env.example not installed"
fi
if [[ ! -f "$ENV_TARGET_REAL" ]]; then
  log "note: $ENV_TARGET_REAL does not exist yet. copy from env.example and fill in secrets before starting services."
fi

# ---- 9. systemd units ----------------------------------------------------

if [[ -d "$SYSTEMD_SRC_DIR" ]]; then
  log "installing systemd units"
  for unit in "$SYSTEMD_SRC_DIR"/*.service; do
    [[ -e "$unit" ]] || continue
    install -m 644 "$unit" "$SYSTEMD_TARGET_DIR/$(basename "$unit")"
  done
  systemctl daemon-reload
else
  log "warning: $SYSTEMD_SRC_DIR not found — systemd units not installed"
fi

# ---- 10. Next steps -------------------------------------------------------

cat <<EOF

================================================================================
[bootstrap] done. agent-collab is installed but not started.

Next steps:

  1. Create the secrets file:
       sudo cp $ENV_TARGET_EXAMPLE $ENV_TARGET_REAL
       sudo chown root:$AC_USER $ENV_TARGET_REAL
       sudo chmod 640 $ENV_TARGET_REAL
       sudo nano $ENV_TARGET_REAL    # fill ANTHROPIC_API_KEY etc.

  2. Configure at least one daemon instance:
       sudo -u $AC_USER cp \\
         $AC_HOME/.agent-collab/daemons/example.json.disabled \\
         $AC_HOME/.agent-collab/daemons/alpha.json
       sudo -u $AC_USER nano $AC_HOME/.agent-collab/daemons/alpha.json

  3. Start services:
       sudo systemctl enable --now agent-collab-peer-web.service
       sudo systemctl enable --now agent-collab-daemon@alpha.service

  4. Verify:
       sudo systemctl status agent-collab-peer-web.service
       curl http://127.0.0.1:8787/api/scope
       sudo journalctl -u agent-collab-daemon@alpha.service -f

Remote access: install Tailscale (\`curl -fsSL https://tailscale.com/install.sh | sh\`),
run \`sudo tailscale up\`, then open the server's tailscale hostname:8787
from any device on your tailnet.

Upgrade: \`cd $PROJECT_ROOT && git pull && sudo bash scripts/server-bootstrap.sh\`
(idempotent — safe to re-run).
================================================================================
EOF
