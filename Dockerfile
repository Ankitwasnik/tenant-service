# One image, every binary under cmd/ (DESIGN.md §9).

FROM golang:1.27 AS build
WORKDIR /src

# Dependencies first, so source-only changes reuse this layer.
# go.sum* matches nothing until the first dependency is added.
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Static binaries (no cgo), so they run on alpine's musl libc.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# alpine rather than distroless: compose health checks use its wget (DESIGN.md §10).
FROM alpine:3.24
RUN adduser -D -H -u 10001 app
USER app
COPY --from=build /out/ /usr/local/bin/
CMD ["controlplane"]
