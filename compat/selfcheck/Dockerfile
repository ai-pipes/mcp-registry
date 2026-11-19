# The image under the suite's self-check: the reference image with a reverse proxy in front of the
# application. BREAK (environment of the container) selects one distortion; unset = pass-through.
# Build: docker build -t mcp-registry:compat-broken -f compat/selfcheck/Dockerfile compat/selfcheck
ARG BASE=mcp-registry:compat-ts
FROM ${BASE}
COPY --chown=node:node proxy.mjs entrypoint.sh /selfcheck/
ENTRYPOINT ["/selfcheck/entrypoint.sh"]
CMD ["node", "dist/main.js"]
