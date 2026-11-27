# The image under the suite's self-check: the reference image plus a distorting reverse proxy.
# BREAK (environment of the container) selects one distortion, or "passthrough" for the proxy
# with none; with BREAK unset the entrypoint starts the application alone, as in the reference
# image, and the proxy is not involved.
# The default base is the reference: the published 1.0.1 pinned by digest (the same value as
# REFERENCE_IMAGE in the Makefile and defaultReferenceImage in compat/harness; a test keeps them equal).
# Build: docker build -t mcp-registry:compat-broken -f compat/selfcheck/Dockerfile compat/selfcheck
ARG BASE=ghcr.io/ai-pipes/mcp-registry:1.0.1@sha256:42726e227e5c2522b35c28c41e1feb6ceb8471ba3447ec5399a6c0559a72baf9
FROM ${BASE}
COPY --chown=node:node proxy.mjs entrypoint.sh /selfcheck/
ENTRYPOINT ["/selfcheck/entrypoint.sh"]
CMD ["node", "dist/main.js"]
