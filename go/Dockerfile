# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
ARG SOURCE_DATE_EPOCH=0
#
# Build the console before compiling Go so the final binary embeds only local assets.
FROM --platform=$BUILDPLATFORM node:22.23.3-alpine@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS console-builder
ARG SOURCE_DATE_EPOCH
WORKDIR /console
COPY internal/web/console/package.json internal/web/console/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY internal/web/console/ ./
RUN npm run build

# ---- builder ------------------------------------------------------------- #
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS builder
WORKDIR /src

# Set by buildx for each requested platform.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE_DATE_EPOCH

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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE_DATE_EPOCH
LABEL org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT \
      org.opencontainers.image.created=$BUILD_TIME

ENV LLMGW_HOST=0.0.0.0 \
    LLMGW_PORT=8787 \
    LLMGW_STATE_DIR=/state \
    LLMGW_PROVIDER_ROSTER_URL=https://xibodev.github.io/llm-gateway/roster/payload.json

COPY --from=builder /out/llmgw /llmgw
COPY --chown=65532:65532 --from=builder /out/state /state

EXPOSE 8787
VOLUME ["/state"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=5 \
    CMD ["/llmgw", "health"]

ENTRYPOINT ["/llmgw"]
CMD ["serve"]
