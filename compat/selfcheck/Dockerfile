# The image under the suite's self-check: the reference image plus a distorting reverse proxy.
# BREAK (environment of the container) selects one distortion, or "passthrough" for the proxy
# with none; with BREAK unset the entrypoint starts the application alone, as in the reference
# image, and the proxy is not involved.
# Build: docker build -t mcp-registry:compat-broken -f compat/selfcheck/Dockerfile compat/selfcheck
ARG BASE=mcp-registry:compat-ts
FROM ${BASE}
COPY --chown=node:node proxy.mjs entrypoint.sh /selfcheck/
ENTRYPOINT ["/selfcheck/entrypoint.sh"]
CMD ["node", "dist/main.js"]
