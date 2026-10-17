# SPDX-License-Identifier: AGPL-3.0-only
# Voice: pure Go (CGO off), static binary, no shell, non-root. No private modules,
# no local files: sessions live in memory only.
ARG GO_IMAGE=docker.io/library/golang:1.26-bookworm
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS TARGETARCH
ENV GOFLAGS=-buildvcs=false
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/voice ./cmd/voice

FROM scratch
COPY --from=build /out/voice /voice
USER 10001:10001
ENTRYPOINT ["/voice"]
