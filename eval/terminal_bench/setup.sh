#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates curl git tar gzip

GO_VERSION="${GO_VERSION:-1.26.2}"
ARCH="$(dpkg --print-architecture)"
case "$ARCH" in
  amd64) GO_ARCH="amd64" ;;
  arm64) GO_ARCH="arm64" ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

if ! command -v go >/dev/null 2>&1; then
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
fi

export GOPATH="${GOPATH:-/root/go}"
export PATH="/usr/local/go/bin:${GOPATH}/bin:${PATH}"

if [ -n "${GRATEFUL_AGENT_INSTALL_CMD:-}" ]; then
  sh -lc "$GRATEFUL_AGENT_INSTALL_CMD"
else
  go install github.com/gratefulagents/sdk/cmd/grateful-agent-run@latest
fi

install -m 0755 "${GOPATH}/bin/grateful-agent-run" /usr/local/bin/grateful-agent-run
