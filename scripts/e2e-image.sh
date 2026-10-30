#!/usr/bin/env bash
# Browser tests (Playwright, e2e/) against the built docker image: builds and starts the compose
# box as its own project on its own port, runs `playwright test` with E2E_BASE_URL in the
# environment, then `docker compose down -v`. Extra arguments go to `playwright test`.
# Needs docker, `pnpm install` and `pnpm exec playwright install chromium`. E2E_BROWSERS (comma
# list, default chromium) adds firefox/webkit projects; install those browsers the same way.
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT="${E2E_PROJECT:-mcp-e2e}"
PORT="${E2E_PORT:-38081}"
BASE_URL="http://localhost:${PORT}"
HEALTH_TIMEOUT=120

ENV_FILE="$(mktemp "${TMPDIR:-/tmp}/mcp-e2e-env.XXXXXX")"
export ENV_FILE
cat >"$ENV_FILE" <<EOF2
PORT=${PORT}
PUBLIC_URL=${BASE_URL}
COOKIE_SECURE=false
POSTGRES_PASSWORD=$(openssl rand -hex 24)
SECRETS_KEY=$(openssl rand -base64 32)
OIDC_ISSUER=https://id.example.invalid
OIDC_CLIENT_ID=e2e
OIDC_CLIENT_SECRET=e2e
LOG_LEVEL=warn
EOF2

compose() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" "$@"; }

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then compose logs --no-color --tail 80 app || true; fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$ENV_FILE"
  exit "$code"
}
trap cleanup EXIT

compose up -d --build

deadline=$((SECONDS + HEALTH_TIMEOUT))
until [ "$(curl -fsS "${BASE_URL}/healthz" 2>/dev/null || true)" = "ok" ]; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "FAIL: /healthz did not return ok in ${HEALTH_TIMEOUT}s" >&2
    exit 1
  fi
  sleep 2
done

E2E_BASE_URL="$BASE_URL" pnpm exec playwright test -c e2e/playwright.config.ts "$@"
