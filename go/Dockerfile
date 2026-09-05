# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
ARG SOURCE_DATE_EPOCH=0
#
# Build the console before compiling Go so the final binary embeds only local assets.
FROM --platform=$BUILDPLATFORM node:22.23.2-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32 AS console-builder
ARG SOURCE_DATE_EPOCH
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
    LLMGW_STATE_DIR=/state

COPY --from=builder /out/llmgw /llmgw
COPY --chown=65532:65532 --from=builder /out/state /state

EXPOSE 8787
VOLUME ["/state"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=5 \
    CMD ["/llmgw", "health"]

ENTRYPOINT ["/llmgw"]
CMD ["serve"]
