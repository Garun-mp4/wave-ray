FROM golang:1.27.2-bookworm AS builder

WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/ruview-server ./cmd/ruview-server

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 ruview \
    && useradd --system --uid 10001 --gid ruview --home-dir /app --no-create-home ruview

WORKDIR /app
COPY --from=builder --chown=10001:10001 /out/ruview-server /app/ruview-server
COPY --chown=10001:10001 ui/ /app/ui/
COPY --chown=10001:10001 v2/crates/cog-pose-estimation/cog/artifacts/pose_v1.safetensors /app/share/pose_v1.safetensors
RUN mkdir -p /app/data/models \
    && chown -R 10001:10001 /app/data

USER 10001:10001
EXPOSE 3000 3001 5005/udp
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl --fail --silent http://127.0.0.1:3000/health/live >/dev/null || exit 1

ENTRYPOINT ["/app/ruview-server"]
CMD ["--bind-addr", "0.0.0.0", "--http-port", "3000", "--ws-port", "3001", "--udp-bind-addr", "0.0.0.0", "--udp-port", "5005", "--static-dir", "/app/ui", "--data-dir", "/app/data", "--models-dir", "/app/data/models"]
