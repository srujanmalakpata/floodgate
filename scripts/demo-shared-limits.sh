#!/usr/bin/env bash
# Sends requests alternately to two gateway replicas with the same API key.
# Because both replicas use the same Redis, the limit (10 per 30s on /api/)
# is shared: about 10 requests succeed in total, not 10 per replica.
set -euo pipefail

N="${1:-16}"
KEY="${KEY:-demo-key}"
ok=0; limited=0
for i in $(seq 1 "$N"); do
  port=$(( i % 2 == 1 ? 8081 : 8082 ))
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: ${KEY}" "http://127.0.0.1:${port}/api/demo")
  printf 'request %2d -> replica :%d -> %s\n' "$i" "$port" "$code"
  case "$code" in
    200) ok=$((ok + 1)) ;;
    429) limited=$((limited + 1)) ;;
  esac
done
echo "allowed=${ok} limited=${limited} (limit is 10 per 30s, shared by both replicas)"
