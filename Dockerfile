# syntax=docker/dockerfile:1

# The build stage runs on the build host's platform and cross-compiles, so
# multi-platform images don't need emulation for the Go build.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/onegit ./cmd/onegit

FROM alpine:3.22
# git (+ git-daemon, which ships http-backend on Alpine) is the only runtime
# dependency: onegit shells out to it for every
# repository operation. The repo volume may be owned by another uid on some
# platforms, so trust it explicitly.
RUN apk add --no-cache git git-daemon ca-certificates tzdata \
 && addgroup -S -g 1000 onegit \
 && adduser -S -D -u 1000 -G onegit -h /home/onegit onegit \
 && mkdir -p /data/repo && chown onegit:onegit /data/repo \
 && git config --system safe.directory '*'
COPY --from=build /out/onegit /usr/local/bin/onegit

USER onegit
ENV ONEGIT_REPO_DIR=/data/repo \
    ONEGIT_HTTP_ADDR=:3000
# The git repository is the only on-disk state; everything else lives in
# Postgres, Redis and S3.
VOLUME /data/repo
EXPOSE 3000
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 CMD ["onegit", "healthcheck"]
ENTRYPOINT ["onegit"]
CMD ["serve"]
