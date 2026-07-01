#!/usr/bin/env bash
# Build the agent from the current working tree and install it on a remote
# VPS over SSH. Used during development to roll out unreleased changes
# without waiting for a tagged release — the binary AND the systemd unit /
# sudoers / tmpfiles fragments under systemd/, since a code change that
# needs a matching config change (e.g. a new sudoers rule) is silently
# incomplete if only the binary gets pushed.
#
# Usage:
#   agent/scripts/deploy-dev.sh <ssh-target> [--arch amd64|arm64] [--sudo]
#
# Examples:
#   agent/scripts/deploy-dev.sh root@vps.example.com
#   agent/scripts/deploy-dev.sh me@vps.example.com --sudo
#   agent/scripts/deploy-dev.sh me@vps.example.com --arch arm64 --sudo
#
# --arch is auto-detected from `uname -m` on the target when omitted.
# --sudo prefixes the remote install/restart commands with `sudo` (needed
# when the SSH user is not root).

set -euo pipefail

if [[ $# -lt 1 || "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  sed -n '2,18p' "$0"
  [[ $# -lt 1 ]] && exit 2 || exit 0
fi

TARGET="$1"
shift
ARCH=""
SUDO=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --sudo) SUDO="sudo"; shift ;;
    -h|--help)
      sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ -z "$ARCH" ]]; then
  echo "[deploy] probing remote arch on $TARGET"
  remote_uname="$(ssh "$TARGET" uname -m)"
  case "$remote_uname" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
      echo "unsupported remote arch: $remote_uname" >&2
      echo "pass --arch amd64|arm64 explicitly" >&2
      exit 1 ;;
  esac
fi

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "--arch must be amd64 or arm64, got: $ARCH" >&2; exit 2 ;;
esac

OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
BIN="$OUT/claver-agent"

echo "[deploy] building linux/$ARCH from $ROOT"
(cd "$ROOT" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$BIN" ./cmd/claver-agent)

SERVICE_SRC="$ROOT/systemd/claver-agent.service"
SUDOERS_SRC="$ROOT/systemd/claver-agent-firewall.sudoers"
TMPFILES_SRC="$ROOT/systemd/claver-agent-sudo.tmpfiles.conf"

echo "[deploy] copying binary + systemd/sudoers/tmpfiles fragments to $TARGET:/tmp"
scp -q "$BIN" "$SERVICE_SRC" "$SUDOERS_SRC" "$TMPFILES_SRC" "$TARGET:/tmp/"

echo "[deploy] installing binary + config fragments on $TARGET"
ssh "$TARGET" "
  set -euo pipefail
  $SUDO install -m 0755 /tmp/claver-agent /usr/local/bin/claver-agent
  $SUDO install -m 0644 /tmp/claver-agent.service /etc/systemd/system/claver-agent.service
  $SUDO install -m 0644 /tmp/claver-agent-sudo.tmpfiles.conf /etc/tmpfiles.d/claver-agent-sudo.conf
  $SUDO systemd-tmpfiles --create /etc/tmpfiles.d/claver-agent-sudo.conf
  $SUDO install -m 0440 /tmp/claver-agent-firewall.sudoers /etc/sudoers.d/claver-agent-firewall.new
  if $SUDO visudo -c -f /etc/sudoers.d/claver-agent-firewall.new >/dev/null; then
    $SUDO mv /etc/sudoers.d/claver-agent-firewall.new /etc/sudoers.d/claver-agent-firewall
  else
    echo 'warning: claver-agent-firewall sudoers fragment failed visudo check; leaving previous fragment in place' >&2
    $SUDO rm -f /etc/sudoers.d/claver-agent-firewall.new
  fi
  $SUDO systemctl daemon-reload
  $SUDO systemctl restart claver-agent
  rm -f /tmp/claver-agent /tmp/claver-agent.service /tmp/claver-agent-firewall.sudoers /tmp/claver-agent-sudo.tmpfiles.conf
"

echo "[deploy] installed version on $TARGET:"
ssh "$TARGET" "/usr/local/bin/claver-agent --version"

echo "[deploy] ok"
