#!/usr/bin/env bash
# build-proto.sh — regenerate peer-inbox protocol bindings from docs/protocol/v1.proto.
#
# Generated Go bindings live at go/pkg/protocol/ and ARE committed to the
# repo so day-to-day development does not require protoc locally.
#
# Required tools:
#   - protoc           (apt install protobuf-compiler | brew install protobuf)
#   - protoc-gen-go    (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest)

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROTO_FILE="$ROOT/docs/protocol/v1.proto"
GO_OUT="$ROOT/go/pkg/protocol"

command -v protoc >/dev/null 2>&1 || {
  echo "error: protoc not on PATH (install from https://grpc.io/docs/protoc-installation/)" >&2
  exit 1
}
command -v protoc-gen-go >/dev/null 2>&1 || {
  echo "error: protoc-gen-go not on PATH (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest)" >&2
  exit 1
}

mkdir -p "$GO_OUT"

protoc \
  --proto_path="$ROOT/docs/protocol" \
  --go_out="$GO_OUT" \
  --go_opt=paths=source_relative \
  --go_opt=Mv1.proto=agent-collaboration/go/pkg/protocol \
  "$PROTO_FILE"

echo "regenerated $GO_OUT/v1.pb.go from $PROTO_FILE"
