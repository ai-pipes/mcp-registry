#!/usr/bin/env bash
# End-to-end test of quickstart.compose.yml, fed through stdin exactly as the one-liner does:
#   build the image from this tree → `docker compose -f - up -d` → /healthz and the image's
#   HEALTHCHECK reports healthy → exactly one
#   first-start sign-in link in the app log → sign in with it (cookie) → /api/auth/me is an admin
#   → create an API token → tools/list on /mcp with it → restart without removing volumes: same
#   token still works, no new link → replace only the secrets key in the `secrets` volume: the
#   app must refuse to start with the fatal text and leak neither key → stack and volumes removed.
# Own compose project and port, so it never touches another deployment. With
# QUICKSTART_IMAGE=<image> the stack runs that prebuilt image (nothing is built, the image is
# never removed). QUICKSTART_CLI is how the operator CLI is invoked inside the app container
# (default `mcp-registry`; the release workflow passes its IMAGE_CLI):
#   QUICKSTART_IMAGE=mcp-registry:compat bash scripts/quickstart-smoke.sh
# Needs docker, curl, openssl. The link code, cookie, token and keys are never printed.
set -euo pipefail

cd "$(dirname "$0")/.."

# The compose file passes these through from the environment: a developer's shell must not change the run.
unset OIDC_ISSUER OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_LOGIN_GROUP OIDC_ADMIN_GROUP OIDC_SCOPES OIDC_GROUPS_CLAIM ALLOWED_HOSTS TRUST_PROXY TRUST_PROXY_HOPS COOKIE_SECURE SYNC_INTERVAL_HOURS CALL_LOG_RETENTION_DAYS UPSTREAM_MAX_RESPONSE_BYTES MCP_MAX_BODY_BYTES EMBEDDER EMBEDDER_URL EMBEDDER_MODEL SEARCH_MIN_SIMILARITY LOG_LEVEL

PROJECT="${QUICKSTART_PROJECT:-mcp-quickstart-smoke}"
PORT="${QUICKSTART_PORT:-38081}"
BASE_URL="http://localhost:${PORT}"
# A prebuilt image (the release workflow's candidate) is used as it is and left in place.
if [ -n "${QUICKSTART_IMAGE:-}" ]; then
  IMAGE="$QUICKSTART_IMAGE"
  BUILD_IMAGE=""
else
  IMAGE="mcp-registry:quickstart-smoke"
  BUILD_IMAGE=1
fi
# The CLI command, split into words (no shell in between).
read -r -a CLI <<<"${QUICKSTART_CLI:-mcp-registry}"
[ "${#CLI[@]}" -gt 0 ] || { echo "QUICKSTART_CLI is empty" >&2; exit 2; }
HEALTH_TIMEOUT=120
CONTAINER_HEALTH_TIMEOUT=90
MISMATCH_TIMEOUT=60
FATAL_TEXT='SECRETS_KEY does not match the data in this database; restore the original key (stored secrets cannot be read with this one)'
FATAL_HINT='Recovering from a key mismatch'

export MCP_REGISTRY_IMAGE="$IMAGE" PORT PUBLIC_URL="$BASE_URL"

# The stack is described only by quickstart.compose.yml on stdin, as in the documented command.
# `--env-file /dev/null` keeps a `.env` in the repository root (which Compose would read) out of the run.
compose() { docker compose --env-file /dev/null -p "$PROJECT" -f - "$@" <quickstart.compose.yml; }
# The documented file-less form, for commands after the stack is up (no -f, no env file).
pcompose() { docker compose -p "$PROJECT" "$@"; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mcp-quickstart-smoke.XXXXXX")"

# The first-start log record holds the live sign-in link: it never reaches this script's output.
redact() { sed -E 's/link=ll_[A-Za-z0-9_-]+/link=<redacted>/g'; }

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    echo "--- app logs (last 80 lines; sign-in link code redacted) ---"
    compose logs --no-color --tail 80 app 2>&1 | redact || true
    echo "--- db logs (last 20 lines) ---"
    compose logs --no-color --tail 20 db 2>&1 | redact || true
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  [ -z "$BUILD_IMAGE" ] || docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
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

# The image's own HEALTHCHECK must turn the container healthy (bounded). On a timeout the status
# and the last health log entries are printed (the probe, `mcp-registry healthcheck`, prints one
# short line and never a secret).
wait_container_healthy() {
  local id status deadline=$((SECONDS + CONTAINER_HEALTH_TIMEOUT))
  id="$(compose ps -q app)"
  [ -n "$id" ] || fail "no app container"
  while :; do
    status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}no healthcheck{{end}}' "$id" 2>/dev/null || echo unknown)"
    [ "$status" = "healthy" ] && break
    if [ "$SECONDS" -ge "$deadline" ] || [ "$status" = "no healthcheck" ]; then
      echo "container health: ${status}; last checks (exit code, output):" >&2
      docker inspect -f '{{if .State.Health}}{{range .State.Health.Log}}{{.ExitCode}} {{printf "%q" .Output}}{{println}}{{end}}{{end}}' "$id" 2>/dev/null | tail -n 5 >&2 || true
      fail "the app container did not report healthy in ${CONTAINER_HEALTH_TIMEOUT}s (HEALTHCHECK of the image)"
    fi
    sleep 2
  done
  echo "container health: healthy"
}


app_log() { compose logs --no-color --no-log-prefix app 2>&1; }

# Rendered configuration of the app service for the current environment, captured (not piped into
# `grep -q`, which can end the producer early and fail under pipefail).
rendered() { compose config 2>&1; }

wait_healthy() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  until [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "/healthz did not return ok in ${HEALTH_TIMEOUT}s"
    sleep 2
  done
  echo "healthz: ok"
  wait_container_healthy
}

# tools/list on /mcp with $1 as the Bearer token; prints nothing, succeeds only with the 4 meta-tools.
mcp_tools_ok() {
  local out
  (umask 077 && printf 'Authorization: Bearer %s\n' "$1" >"$WORK/auth.hdr")
  out="$(curl -sS -X POST "${BASE_URL}/mcp" \
    -H @"$WORK/auth.hdr" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}')" || return 1
  for t in list_servers search_tools describe_tool call_tool; do
    grep -q "\"name\":\"${t}\"" <<<"$out" || return 1
  done
}

step "pass-through and defaults of the rendered configuration"
cfg="$(export LOG_LEVEL=debug; rendered)"
grep -qE '^ +LOG_LEVEL: debug$' <<<"$cfg" || fail "LOG_LEVEL from the environment did not reach the app service"
cfg="$(rendered)"
grep -qE '^ +LOG_LEVEL: ""$' <<<"$cfg" || fail "an unset optional variable is not empty in the app service"
grep -qE '^ +SECRETS_KEY_OLD: ""$' <<<"$cfg" || fail "SECRETS_KEY_OLD is not passed through (empty) to the app service"
grep -qE '^ +host_ip: 127\.0\.0\.1$' <<<"$cfg" || fail "the port is not published on 127.0.0.1 by default"
cfg="$(unset MCP_REGISTRY_IMAGE; rendered)"
grep -qE '^ +image: ghcr\.io/ai-pipes/mcp-registry:2$' <<<"$cfg" ||
  fail "the default image is not ghcr.io/ai-pipes/mcp-registry:2"
cfg="$(unset PUBLIC_URL; export PORT=38081 BIND_ADDRESS=0.0.0.0; rendered)"
grep -qE '^ +PUBLIC_URL: http://localhost:38081$' <<<"$cfg" || fail "PORT without PUBLIC_URL did not give PUBLIC_URL http://localhost:<PORT>"
grep -qE '^ +host_ip: 0\.0\.0\.0$' <<<"$cfg" || fail "BIND_ADDRESS did not change the published address"
echo "config: pass-through, 127.0.0.1 by default, PUBLIC_URL follows PORT, default image"

if [ -n "$BUILD_IMAGE" ]; then
  step "docker build (${IMAGE})"
  docker build -q -t "$IMAGE" . >/dev/null
else
  step "prebuilt image ${IMAGE}"
  docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "prebuilt image ${IMAGE} not found"
fi

step "docker compose -f - up -d (quickstart.compose.yml on stdin)"
compose up -d

step "waiting for ${BASE_URL}/healthz (up to ${HEALTH_TIMEOUT}s)"
wait_healthy

step "the first log record is 'starting' with the version"
first_record="$(app_log | grep -m1 '"msg"' || true)"
grep -q '"msg":"starting"' <<<"$first_record" || fail "the first app log record is not 'starting'"
version="$(tr -d '[:space:]' < VERSION)"
grep -qF "\"version\":\"${version}\"" <<<"$first_record" || fail "the 'starting' record has no version ${version}"
echo "starting: version ${version}"

step "the first-start sign-in link is logged exactly once"
link_records="$(app_log | grep -c '/login#link=ll_' || true)"
[ "$link_records" = "1" ] || fail "expected exactly 1 log record with a sign-in link, got ${link_records}"
code="$(app_log | grep '/login#link=ll_' | sed -E 's/.*#link=(ll_[A-Za-z0-9_-]+).*/\1/')"
[[ "$code" =~ ^ll_ ]] || fail "could not extract the link code"
echo "link: 1 record"

step "POST /api/auth/link → 204 and a session cookie"
(umask 077 && printf '{"code":"%s"}' "$code" >"$WORK/link.json")
status="$(curl -sS -o /dev/null -D "$WORK/headers" -w '%{http_code}' -X POST "${BASE_URL}/api/auth/link" \
  -H "Origin: ${BASE_URL}" -H 'Content-Type: application/json' --data @"$WORK/link.json")"
[ "$status" = "204" ] || fail "POST /api/auth/link answered ${status}, expected 204"
# curl does not send a Secure cookie over plain http, so the name=value pair is passed by hand.
cookie="$(grep -i '^set-cookie:' "$WORK/headers" | head -n 1 | sed -E 's/^[^:]+: *//; s/;.*//' | tr -d '\r')"
[ -n "$cookie" ] || fail "no session cookie in the response"
(umask 077 && printf 'Cookie: %s\n' "$cookie" >"$WORK/cookie.hdr")
grep -i '^set-cookie:' "$WORK/headers" | grep -qi 'httponly' || fail "session cookie is not HttpOnly"
grep -i '^set-cookie:' "$WORK/headers" | grep -qi '; *secure' ||
  fail "session cookie is not Secure by default"
echo "signed in (cookie: HttpOnly, Secure)"

step "GET /api/auth/me → admin"
me="$(curl -fsS "${BASE_URL}/api/auth/me" -H @"$WORK/cookie.hdr")" || fail "GET /api/auth/me failed"
grep -q '"role":"admin"' <<<"$me" || fail "the signed-in person is not an admin"
echo "me: admin"

step "documented file-less commands: login-link through -p, logs through -p"
# Exactly what the compose header and the README tell people to type (no -f, no env file).
link_out="$(pcompose exec -T app "${CLI[@]}" login-link local-admin 2>"$WORK/login-link.err")" ||
  fail "login-link local-admin exited non-zero"
grep -qE "^${BASE_URL}/login#link=ll_[A-Za-z0-9_-]+$" <<<"$link_out" ||
  fail "login-link did not print a link of the expected shape"
unset link_out
logs_out="$(pcompose logs app 2>&1)" || fail "docker compose -p logs app failed"
grep -q 'one-time link' <<<"$logs_out" || fail "no 'one-time link' record in the logs read through -p"
unset logs_out
echo "login-link: exit 0, link shape ok (redacted); logs through -p: ok"

step "POST /api/tokens → sk_ token; tools/list on /mcp"
created="$(curl -fsS -X POST "${BASE_URL}/api/tokens" -H @"$WORK/cookie.hdr" \
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

step "a database migrated by a newer version: the app must refuse to start"
pcompose exec -T db psql -U mcp -d mcp -qc \
  "insert into platform.schema_migrations (name) values ('9999_from_the_future.sql')" >/dev/null
compose up -d --force-recreate --no-deps app
deadline=$((SECONDS + MISMATCH_TIMEOUT))
seen=""
while [ "$SECONDS" -lt "$deadline" ] && [ -z "$seen" ]; do
  if [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; then
    fail "the app became healthy on a database from a newer version"
  fi
  grep -qF 'migrated by a newer version of mcp-registry (unknown migration: 9999_from_the_future.sql)' \
    <<<"$(app_log || true)" && seen=1
  [ -n "$seen" ] || sleep 3
done
[ -n "$seen" ] || fail "the app log has no fatal record about the newer database"
grep -qF 'restore the backup taken before the upgrade' <<<"$(app_log)" ||
  fail "the fatal record does not say how to recover"
pcompose exec -T db psql -U mcp -d mcp -qc \
  "delete from platform.schema_migrations where name = '9999_from_the_future.sql'" >/dev/null
compose up -d --force-recreate --no-deps app
wait_healthy
mcp_tools_ok "$token" || fail "the token does not work after the newer-database check"
echo "newer database: refused with a fatal record; healthy again after the row is removed"

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
  log="$(app_log || true)"
  if grep -qF "$FATAL_TEXT" <<<"$log"; then seen=1; fi
  sleep 3
done
[ -n "$seen" ] || fail "the app log has no fatal record about the key mismatch"
log="$(app_log)"
grep -qF "$FATAL_HINT" <<<"$log" || fail "the fatal record does not point to the recovery section"
grep -qF "${old_key%$'\n'}" <<<"$log" && fail "the app log contains the old key"
grep -qF "$new_key" <<<"$log" && fail "the app log contains the new key"
echo "mismatch: not healthy, fatal record logged, no key in the log"
