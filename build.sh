#!/bin/bash
set -e
cd "$(dirname "$0")"

go build -o relayscheduler .
echo "Built relayscheduler binary."

/Applications/Relay.app/Contents/MacOS/relay service register \
  --name "Relay Scheduler" \
  --command "$(pwd)/relayscheduler" \
  --url http://localhost:3002 \
  --autostart
echo ""
echo "Registered with Relay."
