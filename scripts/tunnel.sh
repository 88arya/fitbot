#!/bin/bash
# Exposes a local port through a cloudflared quick tunnel and points the
# Telnyx messaging profile's webhook at it. Usage: scripts/tunnel.sh [port]
set -euo pipefail

cd "$(dirname "$0")/.."
set -a
. ./.env
set +a
: "${TELNYX_API_KEY:?}" "${TELNYX_FROM_NUMBER:?}"

port=${1:-${HTTP_ADDR##*:}}
api=https://api.telnyx.com/v2
auth="Authorization: Bearer $TELNYX_API_KEY"

profile=$(curl -sSf -H "$auth" "$api/phone_numbers?filter%5Bphone_number%5D=${TELNYX_FROM_NUMBER/+/%2B}" |
  grep -oE '"messaging_profile_id": *"[^"]+"' | head -1 | cut -d'"' -f4)
[ -n "$profile" ] || { echo "no messaging profile for $TELNYX_FROM_NUMBER" >&2; exit 1; }

log=$(mktemp)
cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:$port" > "$log" 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; rm -f "$log"' EXIT

url=
for _ in $(seq 1 30); do
  url=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$log" | head -1 || true)
  [ -n "$url" ] && break
  sleep 1
done
[ -n "$url" ] || { echo "tunnel did not start" >&2; cat "$log" >&2; exit 1; }

curl -sSf -o /dev/null -X PATCH "$api/messaging_profiles/$profile" -H "$auth" \
  -H "Content-Type: application/json" -d "{\"webhook_url\":\"$url/webhooks/telnyx\"}"
echo "webhook: $url/webhooks/telnyx -> localhost:$port"

wait $pid
