#!/bin/sh
# Entrypoint of the self-check image. BREAK unset: the application runs as in the reference image.
# BREAK set (a distortion name, or "passthrough"): the application listens on SELFCHECK_APP_PORT and
# the proxy (compat/selfcheck/proxy.mjs) on the port the harness publishes. The container's exit
# code is the application's, and a stop signal is handed to the application.
set -u

if [ -z "${BREAK:-}" ]; then
  exec "$@"
fi

public_port="${PORT:-3000}"
app_port="${SELFCHECK_APP_PORT:-3001}"

PORT="$app_port" "$@" &
app=$!
SELFCHECK_LISTEN_PORT="$public_port" SELFCHECK_TARGET_PORT="$app_port" node /selfcheck/proxy.mjs &
proxy=$!

trap 'kill -TERM "$app" 2>/dev/null' TERM INT

code=0
wait "$app" || code=$?
# A trapped signal ends wait early; the application may still be shutting down.
while kill -0 "$app" 2>/dev/null; do
  wait "$app" || code=$?
done
kill -TERM "$proxy" 2>/dev/null
wait "$proxy" 2>/dev/null
exit "$code"
