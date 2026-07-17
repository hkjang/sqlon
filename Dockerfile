# syntax=docker/dockerfile:1
# SQLON standard build (PostgreSQL/MySQL/MariaDB). Oracle is distributed as a
# separate CGO-enabled sqlon-oracle image with Oracle Instant Client.
# Metadata (data/metadb) is baked in; mount a volume over /app/data/metadb to
# override, and mount /app/data/metadb/feedback + audit for persistence.

FROM golang:1.26-alpine AS build
WORKDIR /src
# VERSION is injected into mcp.Version so the running server/UI report the
# release tag. Pass with: docker build --build-arg VERSION=0.36.0 .
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN LDFLAGS="-s -w -X sqlon/internal/mcp.Version=${VERSION}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/sqlon ./cmd/jamypg-mcp \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/jamypg-eval ./cmd/jamypg-eval \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/jamypg-goldgen ./cmd/jamypg-goldgen

FROM alpine:3.21
RUN adduser -D -u 10001 sqlon
COPY --from=build /out/sqlon /out/jamypg-eval /out/jamypg-goldgen /usr/local/bin/
COPY --chown=sqlon:sqlon data/metadb /app/data/metadb
WORKDIR /app
USER sqlon
EXPOSE 9797
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:9797/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["sqlon"]
CMD ["-transport", "http", "-addr", "0.0.0.0:9797", "-public-mcp", "-data", "/app/data/metadb"]
