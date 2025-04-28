# syntax = docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS TARGETARCH

WORKDIR /go/src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN	--mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
	GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -ldflags="-extldflags '-static'" -o /usr/local/bin/swarm-tasks-exporter 

FROM alpine:3.22

COPY --from=builder /usr/local/bin/swarm-tasks-exporter /usr/local/bin/swarm-tasks-exporter

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://localhost:8888/metrics || exit 1

ENTRYPOINT ["/usr/local/bin/swarm-tasks-exporter"]
