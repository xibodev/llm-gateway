# syntax=docker/dockerfile:1
#
# Build the console before compiling Go so the final binary embeds only local assets.
FROM --platform=$BUILDPLATFORM node:22-alpine AS console-builder
WORKDIR /console
COPY internal/web/console/package.json internal/web/console/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY internal/web/console/ ./
RUN npm run build

# ---- builder ------------------------------------------------------------- #
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS builder
WORKDIR /src

# Set by buildx for each requested platform.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=console-builder /console/dist ./internal/web/console/dist
# Static, stripped binary. The generated console is embedded at compile time.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w \
      -X=llmgw/internal/buildinfo.Version=${VERSION} \
      -X=llmgw/internal/buildinfo.Commit=${COMMIT} \
      -X=llmgw/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/llmgw ./cmd/llmgw \
    && mkdir -p /out/state

# ---- runtime -------------------------------------------------------------- #
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
LABEL org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT \
      org.opencontainers.image.created=$BUILD_TIME

ENV LLMGW_HOST=0.0.0.0 \
    LLMGW_PORT=8787 \
    LLMGW_STATE_DIR=/state

COPY --from=builder /out/llmgw /llmgw
COPY --chown=65532:65532 --from=builder /out/state /state

EXPOSE 8787
VOLUME ["/state"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=5 \
    CMD ["/llmgw", "health"]

ENTRYPOINT ["/llmgw"]
CMD ["serve"]
