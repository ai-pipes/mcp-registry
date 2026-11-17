#!/usr/bin/env bash
# End-to-end test of quickstart.compose.yml, fed through stdin exactly as the one-liner does:
#   build the image from this tree → `docker compose -f - up -d` → /healthz → exactly one
#   first-start sign-in link in the app log → sign in with it (cookie) → /api/auth/me is an admin
#   → create an API token → tools/list on /mcp with it → restart without removing volumes: same
#   token still works, no new link → replace only the secrets key in the `secrets` volume: the
#   app must refuse to start with the fatal text and leak neither key → stack and volumes removed.
# Own compose project and port, so it never touches another deployment. Needs docker, curl,
# openssl. The link code, cookie, token and keys are never printed.
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT="${QUICKSTART_PROJECT:-mcp-quickstart-smoke}"
PORT="${QUICKSTART_PORT:-38081}"
BASE_URL="http://localhost:${PORT}"
IMAGE="mcp-registry:quickstart-smoke"
HEALTH_TIMEOUT=120
MISMATCH_TIMEOUT=60
FATAL_TEXT='SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one)'

export MCP_REGISTRY_IMAGE="$IMAGE" PORT PUBLIC_URL="$BASE_URL"

# The stack is described only by quickstart.compose.yml on stdin, as in the documented command.
compose() { docker compose -p "$PROJECT" -f - "$@" <quickstart.compose.yml; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mcp-quickstart-smoke.XXXXXX")"

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    echo "--- app logs (last 80 lines; secrets are not logged) ---"
    compose logs --no-color --tail 80 app || true
    echo "--- db logs (last 20 lines) ---"
    compose logs --no-color --tail 20 db || true
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$WORK"
  if [ "$code" -eq 0 ]; then echo "QUICKSTART SMOKE OK"; else echo "QUICKSTART SMOKE FAILED"; fi
  exit "$code"
}
trap cleanup EXIT

step() { echo "==> $*"; }
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

app_log() { compose logs --no-color --no-log-prefix app 2>&1; }

wait_healthy() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  until [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "/healthz did not return ok in ${HEALTH_TIMEOUT}s"
    sleep 2
  done
  echo "healthz: ok"
}

# tools/list on /mcp with $1 as the Bearer token; prints nothing, succeeds only with the 4 meta-tools.
mcp_tools_ok() {
  local out
  out="$(curl -sS -X POST "${BASE_URL}/mcp" \
    -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')" || return 1
  for t in list_servers search_tools describe_tool call_tool; do
    grep -q "\"name\":\"${t}\"" <<<"$out" || return 1
  done
}

step "docker build (${IMAGE})"
docker build -q -t "$IMAGE" . >/dev/null

step "docker compose -f - up -d (quickstart.compose.yml on stdin)"
compose up -d

step "waiting for ${BASE_URL}/healthz (up to ${HEALTH_TIMEOUT}s)"
wait_healthy

step "the first-start sign-in link is logged exactly once"
link_records="$(app_log | grep -c '/login#link=ll_' || true)"
[ "$link_records" = "1" ] || fail "expected exactly 1 log record with a sign-in link, got ${link_records}"
code="$(app_log | grep '/login#link=ll_' | sed -E 's/.*#link=(ll_[A-Za-z0-9_-]+).*/\1/')"
[[ "$code" =~ ^ll_ ]] || fail "could not extract the link code"
echo "link: 1 record"

step "POST /api/auth/link → 204 and a session cookie"
status="$(curl -sS -o /dev/null -D "$WORK/headers" -w '%{http_code}' -X POST "${BASE_URL}/api/auth/link" \
  -H "Origin: ${BASE_URL}" -H 'Content-Type: application/json' -d "{\"code\":\"${code}\"}")"
[ "$status" = "204" ] || fail "POST /api/auth/link answered ${status}, expected 204"
# curl does not send a Secure cookie over plain http, so the name=value pair is passed by hand.
cookie="$(grep -i '^set-cookie:' "$WORK/headers" | head -n 1 | sed -E 's/^[^:]+: *//; s/;.*//' | tr -d '\r')"
[ -n "$cookie" ] || fail "no session cookie in the response"
grep -i '^set-cookie:' "$WORK/headers" | grep -qi 'httponly' || fail "session cookie is not HttpOnly"
grep -i '^set-cookie:' "$WORK/headers" | grep -qi '; *secure' ||
  fail "session cookie is not Secure by default"
echo "signed in (cookie: HttpOnly, Secure)"

step "GET /api/auth/me → admin"
me="$(curl -fsS "${BASE_URL}/api/auth/me" -H "Cookie: ${cookie}")" || fail "GET /api/auth/me failed"
grep -q '"role":"admin"' <<<"$me" || fail "the signed-in person is not an admin"
echo "me: admin"

step "POST /api/tokens → sk_ token; tools/list on /mcp"
created="$(curl -fsS -X POST "${BASE_URL}/api/tokens" -H "Cookie: ${cookie}" \
  -H "Origin: ${BASE_URL}" -H 'Content-Type: application/json' -d '{"name":"quickstart-smoke"}')" ||
  fail "POST /api/tokens failed"
token="$(sed -E 's/.*"token":"(sk_[^"]+)".*/\1/' <<<"$created")"
[[ "$token" =~ ^sk_ ]] || fail "no sk_ token in the response"
mcp_tools_ok "$token" || fail "tools/list on /mcp with the new token failed"
echo "token: created; /mcp tools/list: 4 meta-tools"

step "restart without removing volumes: same token, no new link"
compose down
compose up -d
wait_healthy
mcp_tools_ok "$token" || fail "the token does not work after a restart"
link_records="$(app_log | grep -c '/login#link=ll_' || true)"
# `compose logs` after `down` + `up` shows the new container only: it must have no link at all.
[ "$link_records" = "0" ] || fail "a restart logged ${link_records} new sign-in link record(s)"
echo "restart: token still valid, no new link"

step "replace only the secrets key: the app must refuse to start"
old_key="$(compose run --rm --no-deps -T --entrypoint cat init /secrets/secrets_key 2>/dev/null)" ||
  fail "could not read the current key"
compose down
new_key="$(openssl rand -base64 32)"
[ "$new_key" != "$old_key" ] || fail "new key equals the old one"
# One-off container on the same volume; init's image has a shell and runs as root.
printf '%s\n' "$new_key" |
  docker run --rm -i -v "${PROJECT}_secrets:/secrets" --entrypoint sh pgvector/pgvector:pg17 \
    -c 'cat > /secrets/secrets_key'
compose up -d
deadline=$((SECONDS + MISMATCH_TIMEOUT))
seen=""
while [ "$SECONDS" -lt "$deadline" ]; do
  if [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; then
    fail "the app became healthy with a key that does not match the data"
  fi
  if app_log | grep -qF "$FATAL_TEXT"; then seen=1; fi
  sleep 3
done
[ -n "$seen" ] || fail "the app log has no fatal record about the key mismatch"
log="$(app_log)"
grep -qF "${old_key%$'\n'}" <<<"$log" && fail "the app log contains the old key"
grep -qF "$new_key" <<<"$log" && fail "the app log contains the new key"
echo "mismatch: not healthy, fatal record logged, no key in the log"
