# syntax=docker/dockerfile:1.7
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/talksasa-pay ./cmd/talksasa-pay

# Distroless static: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/talksasa-pay /talksasa-pay
USER nonroot:nonroot
EXPOSE 8088
ENTRYPOINT ["/talksasa-pay"]
