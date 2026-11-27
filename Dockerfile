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
COPY tsconfig.base.json vite.config.ts VERSION ./
COPY packages/contracts packages/contracts
COPY apps/server apps/server
RUN pnpm build
COPY apps/web apps/web
RUN pnpm build:web

# --- web: the web UI alone (apps/web/dist, static files), for the Go image. Only the web client's
#     workspace packages are copied and installed (`--filter @mcp/web...`: the UI and the packages it
#     depends on, @mcp/contracts); apps/server is neither copied nor built. -----------------------
FROM base AS web
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/web/package.json apps/web/
COPY packages/contracts/package.json packages/contracts/
RUN pnpm install --frozen-lockfile --ignore-scripts --filter '@mcp/web...'
COPY tsconfig.base.json ./
COPY packages/contracts packages/contracts
COPY apps/web apps/web
RUN pnpm build:web

# --- deps: production dependencies only (fresh install, so devDependencies never get in) -----
FROM base AS deps
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/server/package.json apps/server/
COPY packages/contracts/package.json packages/contracts/
RUN pnpm install --frozen-lockfile --prod --ignore-scripts

# --- model: the embedding model (q8), pinned revision, every file checked by SHA-256. No Node: the
#     script and the pins are the ones `make native-model` uses (internal/modules/search/infra/onnx/
#     native: fetch-model.sh, pins.env); there is no second copy of a URL or a hash in this file.
#     The golang image (pinned by digest below, the one go-native and go-build use) has curl. The
#     files do not depend on the architecture, so the stage runs on the build platform (no emulation). --
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS model
COPY internal/modules/search/infra/onnx/native/fetch-model.sh internal/modules/search/infra/onnx/native/pins.env /native-src/
RUN sh /native-src/fetch-model.sh /app/models

# --- smoke-upstream: the former TypeScript upstream of scripts/smoke.sh
#     (apps/server/test/smoke/upstream.ts), replaced by the Go stage smoke-go below and kept until the
#     TypeScript server is removed. Nothing builds it; nothing of it reaches the runtime image.
FROM build AS smoke-upstream
USER node
EXPOSE 8080
CMD ["node", "--experimental-strip-types", "--no-warnings", "apps/server/test/smoke/upstream.ts"]

# --- go-native: the native libraries of the local embedder for the target platform (TARGETARCH),
#     from the pinned sources with SHA-256 checks before and after extraction. The script and the
#     list of pins are the ones `make native-libs` uses (internal/modules/search/infra/onnx/native:
#     fetch.sh, pins.env): there is no second copy of a URL or a hash in this file. -------------------
FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-native
ARG TARGETARCH
COPY internal/modules/search/infra/onnx/native/fetch.sh internal/modules/search/infra/onnx/native/pins.env /native-src/
RUN sh /native-src/fetch.sh "linux-${TARGETARCH}" /native \
    && . /native-src/pins.env \
    && eval "sha=\$ORT_SHA_linux_${TARGETARCH}" \
    && echo "${sha}  /usr/local/lib/libonnxruntime.so.1" > /native/ort.sha256

# --- go-build: the Go server, CGO_ENABLED=1 with the build tag onnx (the local embedder), built
#     natively for the target platform. Built only with `--target go` (make compat-image-go); the
#     default target stays `runtime` below, the last stage of this file. ----------------------------------------------------------------------------------
FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY --from=go-native /native/libtokenizers.a /native/libtokenizers.a
COPY cmd cmd
COPY internal internal
# The migrations are embedded from the TypeScript server's folder (apps/server/migrations/embed.go).
COPY apps/server/migrations apps/server/migrations
# The version is the root VERSION file, read here: an image cannot report `dev`, and an empty or
# malformed file fails the build.
COPY VERSION VERSION
RUN v="$(tr -d '[:space:]' < VERSION)" \
    && { echo "$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "VERSION must hold MAJOR.MINOR.PATCH, got '$v'" >&2; exit 1; }; } \
    && CGO_ENABLED=1 CGO_LDFLAGS="-L/native" go build -tags onnx -trimpath -ldflags "-s -w -X main.version=$v" -o /out/mcp-registry ./cmd/mcp-registry

# --- smoke-go: the test-only tools of scripts/smoke.sh in Go (compat/cmd/smoke-upstream, the fake
#     upstream MCP server, and compat/cmd/smoke-client, the MCP client), built with -tags compat. Built
#     only by scripts/smoke.compose.yml; nothing of it reaches an image people run. ------------------
FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS smoke-go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY compat/cmd compat/cmd
RUN CGO_ENABLED=0 go build -tags compat -trimpath -ldflags "-s -w" -o /out/ ./compat/cmd/smoke-upstream ./compat/cmd/smoke-client

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS smoke-go
COPY --from=smoke-go-build /out/smoke-upstream /out/smoke-client /usr/local/bin/
USER 65534:65534
EXPOSE 8080
CMD ["smoke-upstream"]

# --- go: the Go image (not the default target yet: that is `runtime` below, the TypeScript image).
#     No ENTRYPOINT: the server is the default command, which the compat suite replaces to run the
#     CLI in the same image, and `docker run <image> mcp-registry <command>` does the same. The
#     server is PID 1 (exec form, no shell) and handles SIGTERM itself: it stops taking requests,
#     lets the ones in flight finish and exits within its own limit (10 s; give the container at
#     least that, e.g. `stop_grace_period`). The binary is dynamically linked (glibc, libstdc++ for
#     ONNX Runtime). ONNX Runtime sits at onnx.DefaultLibraryPath, the model at the default
#     MODEL_DIR (/app/models, the layout of the TypeScript image: the same `model` stage), the UI at
#     the default web root /app/web (the `web` stage). Everything under /app and /usr/local is owned
#     by root and read-only for the server's user (uid and gid 10001), which needs no writable
#     directory and no home. ----------------------------------------------------------------------
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS go
# glibc keeps freed native memory per thread arena: repeated failed model loads inflated the resident
# memory to gigabytes; with two arenas it plateaus (about 660 MB measured).
ENV MALLOC_ARENA_MAX=2
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libstdc++6 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 mcp \
    && useradd --system --uid 10001 --gid 10001 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin mcp
COPY --from=go-build /out/mcp-registry /usr/local/bin/mcp-registry
COPY --from=go-native --chmod=0755 /native/libonnxruntime.so.1 /usr/local/lib/libonnxruntime.so.1
# A truncated library crashes the process inside dlopen instead of returning an error, so the file
# is checked again at its installed path (the expected hash comes from pins.env through go-native).
RUN --mount=type=bind,from=go-native,source=/native/ort.sha256,target=/tmp/ort.sha256 \
    sha256sum -c /tmp/ort.sha256
COPY --from=model /app/models /app/models
COPY --from=web /app/apps/web/dist /app/web
# The licences: the overview, the project's own licence and the generated third_party/ files
# (tools/notices); the Debian packages carry theirs in /usr/share/doc.
COPY LICENSE THIRD_PARTY_NOTICES /usr/share/licenses/mcp-registry/
COPY third_party /usr/share/licenses/mcp-registry/third_party
LABEL org.opencontainers.image.title="mcp-registry" \
      org.opencontainers.image.description="Self-hosted MCP gateway: one MCP endpoint in front of your upstream MCP servers" \
      org.opencontainers.image.source="https://github.com/ai-pipes/mcp-registry" \
      org.opencontainers.image.licenses="MIT"
USER 10001:10001
EXPOSE 3000
STOPSIGNAL SIGTERM
# The probe is the binary itself (no curl, no Node in the image); it needs no configuration and reads
# only PORT. Same timings as the TypeScript image's HEALTHCHECK.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
  CMD ["mcp-registry", "healthcheck"]
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
