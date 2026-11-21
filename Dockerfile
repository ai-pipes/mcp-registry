# syntax=docker/dockerfile:1

# --- base: Node 22 + the pnpm version from package.json ---------------------------------------
FROM node:22-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee AS base
RUN npm install --global --no-fund --no-audit pnpm@11.16.0
WORKDIR /app

# --- build: full install, bundle apps/server/src into apps/server/dist and the UI into
#     apps/web/dist (static files; no network is needed at runtime) ------------------------------
FROM base AS build
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/server/package.json apps/server/
COPY apps/web/package.json apps/web/
COPY packages/contracts/package.json packages/contracts/
# --ignore-scripts: the only allowed build script is lefthook (git hooks, useless here).
# onnxruntime-node needs no install script: its npm package ships the Linux x64/arm64 binaries
# (the postinstall only fetches optional CUDA files).
RUN pnpm install --frozen-lockfile --ignore-scripts
COPY tsconfig.base.json vite.config.ts ./
COPY packages/contracts packages/contracts
COPY apps/server apps/server
RUN pnpm build
COPY apps/web apps/web
RUN pnpm build:web

# --- deps: production dependencies only (fresh install, so devDependencies never get in) -----
FROM base AS deps
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/server/package.json apps/server/
COPY packages/contracts/package.json packages/contracts/
RUN pnpm install --frozen-lockfile --prod --ignore-scripts

# --- model: Xenova/multilingual-e5-small (q8), pinned revision, checked by SHA-256 -----------
FROM node:22-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee AS model
COPY scripts/download-model.mjs /tmp/download-model.mjs
RUN node /tmp/download-model.mjs /app/models

# --- smoke-upstream: the test-only upstream MCP server of scripts/smoke.sh -------------------
#     (apps/server/test/smoke/upstream.ts). Built only with `--target smoke-upstream` by
#     scripts/smoke.compose.yml; nothing of it reaches the runtime image below.
FROM build AS smoke-upstream
USER node
EXPOSE 8080
CMD ["node", "--experimental-strip-types", "--no-warnings", "apps/server/test/smoke/upstream.ts"]

# --- go-build: the Go server (milestone 1: the platform layer), CGO_ENABLED=0. The cgo build with
#     ONNX Runtime arrives in milestone 4. Built only with `--target go` (make compat-image-go);
#     the default target stays `runtime` below, the last stage of this file. ----------------------
FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
# The migrations are embedded from the TypeScript server's folder (apps/server/migrations/embed.go).
COPY apps/server/migrations apps/server/migrations
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mcp-registry ./cmd/mcp-registry

# --- go: the Go image. No ENTRYPOINT (the compat suite runs the CLI in the same image by replacing
#     the command), no HEALTHCHECK yet (the TypeScript one runs node). ---------------------------
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS go
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin mcp
COPY --from=go-build /out/mcp-registry /usr/local/bin/mcp-registry
COPY --from=build /app/apps/web/dist /app/web
USER 10001
EXPOSE 3000
CMD ["mcp-registry"]

# --- runtime ----------------------------------------------------------------------------------
FROM node:22-slim@sha256:e9f565d32859f0cbfb52125eafe8be39390f24dac302dc3cdf566e8ba3ab21ee AS runtime
ENV NODE_ENV=production \
    PORT=3000 \
    MODEL_DIR=/app/models \
    HF_HUB_OFFLINE=1
WORKDIR /app/apps/server
COPY --from=deps /app/node_modules /app/node_modules
COPY --from=deps /app/apps/server/node_modules /app/apps/server/node_modules
COPY --from=model /app/models /app/models
COPY apps/server/package.json ./
COPY apps/server/migrations ./migrations
COPY --from=build /app/apps/server/dist ./dist
# dist/main.js serves the UI from ../../web/dist, i.e. /app/apps/web/dist.
COPY --from=build /app/apps/web/dist /app/apps/web/dist
USER node
EXPOSE 3000
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
  CMD ["node", "-e", "fetch('http://127.0.0.1:'+(process.env.PORT??3000)+'/healthz').then(r=>r.text()).then(t=>process.exit(t==='ok'?0:1),()=>process.exit(1))"]
CMD ["node", "dist/main.js"]
