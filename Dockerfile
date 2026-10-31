# Release builds must use a Go toolchain on which `govulncheck ./...` is clean (the standard library ships in the binary).
# golang:1.26.10 was verified clean; go.mod's `go 1.24.7` is only the minimum language version, no bump needed.
FROM golang:1.26.10-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly
# dependency layer first: only invalidated when go.mod / go.sum change
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/openetl-server ./cmd/openetl-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && addgroup -S -g 10001 openetl && adduser -S -u 10001 -G openetl -h /data openetl \
 && mkdir -p /data/blobs && chown -R openetl:openetl /data
COPY --from=build /out/openetl-server /usr/local/bin/openetl-server
USER openetl:openetl
# fs blob storage default; mount a volume here (it is part of the backup, see docs/operations.md)
ENV OPENETL_STORAGE_DIR=/data/blobs
VOLUME /data
EXPOSE 8080
# OPENETL_ADDR defaults to :8080; change the port here if you change it
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["openetl-server"]
CMD ["serve"]
