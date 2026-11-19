#!/bin/bash
# Entrypoint of the self-check image. BREAK unset: the application runs as in the reference image
# (no proxy). BREAK set (a distortion name, or "passthrough"): the application listens on
# SELFCHECK_APP_PORT and the proxy (compat/selfcheck/proxy.mjs) on the port the harness publishes.
#
# The container lives exactly as long as BOTH children do: when either exits, the other is stopped
# and the container exits non-zero if the proxy was the one that died (a dead proxy must show up as
# a failed boot, never as a running container with nothing on the public port), else with the
# application's own exit code. A stop signal is handed to both.
set -u

if [ -z "${BREAK:-}" ]; then
  exec "$@"
fi

public_port="${PORT:-3000}"
app_port="${SELFCHECK_APP_PORT:-3001}"
app=""
proxy=""
trap 'kill -TERM $app $proxy 2>/dev/null' TERM INT

PORT="$app_port" "$@" &
app=$!
SELFCHECK_LISTEN_PORT="$public_port" SELFCHECK_TARGET_PORT="$app_port" node /selfcheck/proxy.mjs &
proxy=$!

# Returns when one child has exited (or a trapped signal interrupted the wait).
wait -n "$app" "$proxy"

if ! kill -0 "$proxy" 2>/dev/null; then
  echo "selfcheck entrypoint: the proxy exited; stopping the application" >&2
  kill -TERM "$app" 2>/dev/null
  wait "$app"
  exit 1
fi

kill -TERM "$proxy" 2>/dev/null
# The application's exit code comes from this wait. A signal handled by the trap ends a wait early
# with 128+n while the application still runs, so wait again then.
wait "$app"
code=$?
if kill -0 "$app" 2>/dev/null; then
  wait "$app"
  code=$?
fi
exit "$code"
