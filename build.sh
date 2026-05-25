#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayscheduler .
echo "Built relayscheduler binary."

# Code signing -- mirrors relay's build.sh so hardened-runtime + distribution
# parity stays consistent across every binary spawned by relay.
# RELAY_SIGN_IDENTITY lets you pin a specific cert when multiple are present.
IDENTITY="${RELAY_SIGN_IDENTITY:-$(security find-identity -v -p codesigning | grep "Developer ID Application" | grep -o '"[^"]*"' | head -1 | tr -d '"' || true)}"
if [ -n "$IDENTITY" ]; then
    echo "Signing with: $IDENTITY"
    SIGN_ARGS=(--force --sign "$IDENTITY" --options runtime --timestamp)
else
    echo "No Developer ID found, ad-hoc signing"
    SIGN_ARGS=(--force --sign - --options runtime)
fi
codesign "${SIGN_ARGS[@]}" relayscheduler
codesign --verify --strict --verbose=2 relayscheduler

/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay Scheduler" \
  --command "$(pwd)/relayscheduler" \
  --url http://localhost:3002 \
  --autostart
echo ""
echo "Registered with Relay."
