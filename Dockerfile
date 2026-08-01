# Build a fully static binary. The SQLite driver is pure Go (modernc.org/sqlite)
# and every asset is embedded, so the runtime image needs nothing but the binary.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download is cached across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOFLAGS=-trimpath \
    go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/notiphy ./cmd/notiphy

# --- runtime ---
FROM alpine:3.20

# ca-certificates is needed to reach ntfy over HTTPS and to sign Web Push
# deliveries to Apple's and Google's endpoints. tzdata keeps local timestamps
# on the dashboard honest.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 notiphy && \
    mkdir -p /data && chown notiphy:notiphy /data

COPY --from=build /out/notiphy /usr/local/bin/notiphy

USER notiphy
WORKDIR /data
VOLUME ["/data"]
EXPOSE 8080

ENV NOTIPHY_DB=/data/notiphy.db \
    NOTIPHY_LISTEN=0.0.0.0:8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["notiphy"]
CMD ["serve"]
