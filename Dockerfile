# syntax=docker/dockerfile:1

# --- base: Node 22 + the pnpm version from package.json, for the web UI build only ---------------
FROM node:22-slim@sha256:be979e5523c76fa4d5ca4ebd3099078f220aa47ec39c48ad5b5df9314debd6eb AS base
RUN npm install --global --no-fund --no-audit pnpm@11.16.0
WORKDIR /app

# --- web: the web UI (apps/web/dist, static files; no network is needed at runtime). Only the web
#     client's workspace packages are copied and installed (`--filter @mcp/web...`: the UI and the
#     packages it depends on, @mcp/contracts). Node exists only in this build stage. ---------------
FROM base AS web
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/web/package.json apps/web/
COPY packages/contracts/package.json packages/contracts/
RUN pnpm install --frozen-lockfile --ignore-scripts --filter '@mcp/web...'
COPY tsconfig.base.json ./
COPY packages/contracts packages/contracts
COPY apps/web apps/web
RUN pnpm build:web

# --- model: the embedding model (q8), pinned revision, every file checked by SHA-256. No Node: the
#     script and the pins are the ones `make native-model` uses (internal/modules/search/infra/onnx/
#     native: fetch-model.sh, pins.env); there is no second copy of a URL or a hash in this file.
#     The golang image (pinned by digest below, the one go-native and go-build use) has curl. The
#     files do not depend on the architecture, so the stage runs on the build platform (no emulation). --
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS model
COPY internal/modules/search/infra/onnx/native/fetch-model.sh internal/modules/search/infra/onnx/native/pins.env /native-src/
RUN sh /native-src/fetch-model.sh /app/models

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

# --- go-build: the server, CGO_ENABLED=1 with the build tag onnx (the local embedder), built
#     natively for the target platform (no cross toolchain). ----------------------------------------
FROM golang:1.27-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY --from=go-native /native/libtokenizers.a /native/libtokenizers.a
COPY cmd cmd
COPY internal internal
# The migrations (embedded by migrations/embed.go).
COPY migrations migrations
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

# --- runtime: the image (the last stage, so the default target; docker-compose.yml, the smoke
#     scripts, make compat-image and release.yml build it). No ENTRYPOINT: the server is the default command, which the compat suite replaces to run the
#     CLI in the same image, and `docker run <image> mcp-registry <command>` does the same. The
#     server is PID 1 (exec form, no shell) and handles SIGTERM itself: it stops taking requests,
#     lets the ones in flight finish and exits within its own limit (10 s; give the container at
#     least that, e.g. `stop_grace_period: 15s` as both compose files set). The binary is dynamically linked (glibc, libstdc++ for
#     ONNX Runtime). ONNX Runtime sits at onnx.DefaultLibraryPath, the model at the default
#     MODEL_DIR (/app/models, where the 1.x image had it too), the UI at
#     the default web root /app/web (the `web` stage). Everything under /app and /usr/local is owned
#     by root and read-only for the server's user (uid and gid 1000, as the 1.0.x image), which needs no writable
#     directory and no home. ----------------------------------------------------------------------
FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc AS runtime
# glibc keeps freed native memory per thread arena: repeated failed model loads inflated the resident
# memory to gigabytes; with two arenas it plateaus (about 660 MB measured).
ENV MALLOC_ARENA_MAX=2
# The last step removes the setuid and setgid bits Debian ships (su, passwd, mount, chsh...):
# nothing in the image needs to change user.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libstdc++6 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 1000 mcp \
    && useradd --system --uid 1000 --gid 1000 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin mcp \
    && find / -xdev -perm /6000 -type f -exec chmod a-s {} +
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
USER 1000:1000
EXPOSE 3000
STOPSIGNAL SIGTERM
# The probe is the binary itself (no curl, no Node in the image); it needs no configuration and reads
# only PORT. Same timings as the 1.x image's HEALTHCHECK.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
  CMD ["mcp-registry", "healthcheck"]
CMD ["mcp-registry"]
