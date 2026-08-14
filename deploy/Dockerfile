# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/kvp ./cmd/kvp

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /out/kvp /usr/local/bin/kvp
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kvp"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:9090/readyz | grep -q '"ready"' || exit 1
