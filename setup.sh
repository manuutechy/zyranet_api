#!/usr/bin/env bash
# setup.sh — Install all Go dependencies for zyranet-api
# Run this once after cloning the repo or whenever go.mod changes.

set -e

GOBIN=$(which go 2>/dev/null || echo "")

# Try to find go binary
if [ -z "$GOBIN" ]; then
  # Common Go locations
  for path in /usr/local/go/bin/go "$HOME/go/bin/go" /opt/go/bin/go; do
    if [ -x "$path" ]; then
      GOBIN="$path"
      break
    fi
  done
  # Fallback: find installed Go toolchains
  if [ -z "$GOBIN" ]; then
    GOBIN=$(find "$HOME/go/pkg/mod" -name "go" -type f -executable 2>/dev/null | grep "toolchain" | sort -V | tail -1)
  fi
fi

if [ -z "$GOBIN" ]; then
  echo "ERROR: Go not found. Please install Go from https://go.dev/dl/"
  exit 1
fi

echo "Using Go: $GOBIN"
echo "Running go mod tidy..."
GOPATH="$HOME/go" "$GOBIN" mod tidy

echo ""
echo "Done! Dependencies installed."
echo ""
echo "To start the dev server:"
echo "  cp .env.example .env && nano .env   # fill in your DB and API credentials"
echo "  GOPATH=\$HOME/go $GOBIN run main.go"
