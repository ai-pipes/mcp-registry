#!/usr/bin/env bash
# End-to-end smoke test of the docker compose deployment:
#   build and start (app + Postgres + a fake upstream MCP server, scripts/smoke.compose.yml)
#   → /healthz = ok → the UI at / with its CSP → container checks
#   → CLI: create-user (admin), create-token, create-server (shared_secret; the secret on stdin)
#     and sync → MCP over HTTP with the token: search_tools → describe_tool → call_tool
#   → the upstream got the secret header on every request, the client and the app logs never
#     saw the secret → docker compose down -v.
# Runs as its own compose project with its own env file and port, so it never touches a
# deployment started from .env in the same checkout. Needs docker, openssl, node 22 and
# `pnpm install` (for the MCP client). OIDC is a placeholder: discovery is lazy and the smoke
# never signs in (the UI login needs a real OIDC provider).
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT="${SMOKE_PROJECT:-mcp-smoke}"
PORT="${SMOKE_PORT:-38080}"
BASE_URL="http://localhost:${PORT}"
HEALTH_TIMEOUT=120
SEARCH_TIMEOUT=120
TOOL_REF="smoke.echo"

# The upstream's shared key: it reaches the app only through the CLI's stdin.
SMOKE_UPSTREAM_SECRET="$(openssl rand -hex 24)"
export SMOKE_UPSTREAM_SECRET

ENV_FILE="$(mktemp "${TMPDIR:-/tmp}/mcp-smoke-env.XXXXXX")"
export ENV_FILE
cat >"$ENV_FILE" <<EOF2
PORT=${PORT}
PUBLIC_URL=${BASE_URL}
COOKIE_SECURE=false
POSTGRES_PASSWORD=$(openssl rand -hex 24)
SECRETS_KEY=$(openssl rand -base64 32)
OIDC_ISSUER=https://id.example.invalid
OIDC_CLIENT_ID=smoke
OIDC_CLIENT_SECRET=smoke
LOG_LEVEL=info
EOF2

compose() {
  docker compose -p "$PROJECT" --env-file "$ENV_FILE" \
    -f docker-compose.yml -f scripts/smoke.compose.yml "$@"
}

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    echo "--- app logs (last 80 lines) ---"
    compose logs --no-color --tail 80 app || true
    echo "--- upstream logs (last 40 lines) ---"
    compose logs --no-color --tail 40 upstream || true
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$ENV_FILE"
  if [ "$code" -eq 0 ]; then echo "SMOKE OK"; else echo "SMOKE FAILED"; fi
  exit "$code"
}
trap cleanup EXIT

step() { echo "==> $*"; }
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

step "docker compose up -d --build"
compose up -d --build

step "waiting for ${BASE_URL}/healthz (up to ${HEALTH_TIMEOUT}s)"
deadline=$((SECONDS + HEALTH_TIMEOUT))
until [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "/healthz did not return ok in ${HEALTH_TIMEOUT}s"
  sleep 2
done
echo "healthz: ok"

step "web UI: GET / serves index.html with the CSP header"
# -D - puts the response headers in front of the body.
ui_response="$(curl -fsS -H 'Accept: text/html' -D - "${BASE_URL}/")" || fail "GET / failed"
grep -q '<div id="root"></div>' <<<"$ui_response" || fail "GET / did not return index.html with #root"
ui_csp="$(grep -i '^content-security-policy:' <<<"$ui_response" | tr -d '\r' || true)"
grep -q "default-src 'self'" <<<"$ui_csp" || fail "GET / has no CSP header with default-src 'self'"
echo "ui: index.html; ${ui_csp}"

step "container checks"
uid="$(compose exec -T app id -u)"
[ "$uid" != "0" ] || fail "app runs as root"
echo "app uid: ${uid}"
# `compose port` prints nothing, ":0" or (newer Compose) "invalid IP:0" for an unpublished port;
# `docker port` lists every published port of the container, so it must be empty.
db_port="$(compose port db 5432 2>/dev/null || true)"
[ -z "$db_port" ] || [ "${db_port%:0}" != "$db_port" ] || fail "db port 5432 is published: ${db_port}"
db_published="$(docker port "$(compose ps -q db)")"
[ -z "$db_published" ] || fail "db publishes ports: ${db_published}"
echo "db: no published ports"

cli() { compose exec -T app node dist/cli.js "$@"; }

step "CLI: create-user (admin, as a first SSO login would)"
cli create-user --sub smoke-admin --name "Smoke Admin" --admin
# Idempotent by sub: a second run updates the same person.
cli create-user --sub smoke-admin --name "Smoke Admin" --admin | grep -q '^updated admin ' ||
  fail "create-user is not idempotent by sub"

step "CLI: create-token"
token_output="$(cli create-token smoke-admin --name smoke)"
SMOKE_TOKEN="$(printf '%s\n' "$token_output" | head -n 1)"
[[ "$SMOKE_TOKEN" =~ ^sk_ ]] || fail "create-token did not print an sk_ token"
printf '%s\n' "$token_output" | sed -n '2p' |
  grep -q '^claude mcp add --transport http mcp-registry ' ||
  fail "create-token did not print the claude mcp add command"
echo "token: ${SMOKE_TOKEN:0:10}… (connect command printed)"
export SMOKE_TOKEN

step "CLI: create-server (shared_secret, secret on stdin) and sync"
server_json='{"slug":"smoke","name":"Smoke upstream","description":"Fake upstream of the smoke test",
  "url":"http://upstream:8080/mcp","access":{"kind":"all"},
  "auth":{"kind":"shared_secret","header":"X-Smoke-Key","template":"Key {secret}"}}'
printf '%s' "$server_json" | compose exec -T app sh -c 'cat > /tmp/smoke-server.json'
created="$(printf '%s' "$SMOKE_UPSTREAM_SECRET" |
  cli create-server --json /tmp/smoke-server.json --as smoke-admin 2>&1)" ||
  fail "create-server failed: ${created}"
echo "$created"
grep -qF "$SMOKE_UPSTREAM_SECRET" <<<"$created" && fail "create-server printed the secret"
grep -q '^created server "smoke" (shared_secret, secret set); 1 tool(s)$' <<<"$created" ||
  fail "create-server did not create and sync the server"
cli sync smoke --as smoke-admin | grep -q '^synced "smoke": ' || fail "sync failed"
echo "synced"

step "MCP: search_tools → describe_tool → call_tool (up to ${SEARCH_TIMEOUT}s while the outbox indexes)"
deadline=$((SECONDS + SEARCH_TIMEOUT))
until node --experimental-strip-types --no-warnings apps/server/test/smoke/mcp-client.ts \
  "$BASE_URL" "$TOOL_REF"; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the MCP path did not succeed in ${SEARCH_TIMEOUT}s"
  sleep 3
done

step "upstream got the secret header; nobody else saw the secret"
sleep 1 # the upstream flushes its request log every 200 ms
upstream_log="$(compose logs --no-color upstream)"
grep -q 'request tools/list authorized=true' <<<"$upstream_log" ||
  fail "upstream saw no authorized tools/list"
grep -q 'request tools/call authorized=true' <<<"$upstream_log" ||
  fail "upstream saw no authorized tools/call"
! grep -q 'authorized=false' <<<"$upstream_log" ||
  fail "upstream got a request without the secret header"
compose logs --no-color app | grep -qF "$SMOKE_UPSTREAM_SECRET" &&
  fail "the app logs contain the upstream secret"
echo "upstream: every request carried the key; app logs: no secret"
