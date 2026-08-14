# syntax=docker/dockerfile:1

FROM dhi.io/golang:1.26-dev AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/kvp ./cmd/kvp \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hc ./deploy/hc \
 && mkdir -p /out/app

FROM dhi.io/static:20260611-alpine
COPY --from=builder --chown=65532:65532 /out/kvp /usr/local/bin/kvp
COPY --from=builder --chown=65532:65532 /out/hc /usr/local/bin/hc
COPY --from=builder --chown=65532:65532 /out/app /app
USER 65532:65532
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kvp"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/hc"]
