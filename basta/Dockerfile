# syntax=docker/dockerfile:1

# Go, tags and ldflags match EarthBuild's buildkitd stage at 51fe8fb9 (stock binary reports go1.21.13).
ARG GO_VERSION=1.21.13
ARG ALPINE_VERSION=3.19
ARG BASE_IMAGE=earthbuild/buildkitd:v0.8.18-b0385986@sha256:72f31b547d4dbca1a2084a95c590f3932977facedaad8c3828c22faa87617ff8

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS buildkitd
RUN apk add --no-cache git
WORKDIR /src
ENV GOFLAGS=-mod=vendor CGO_ENABLED=0
ARG TARGETOS TARGETARCH
# Same string as stock so earth does not see a version mismatch.
ARG RELEASE_VERSION=v0.8.18-b0385986
ARG REVISION
RUN --mount=target=. --mount=type=cache,target=/root/.cache --mount=type=cache,target=/go/pkg/mod <<'SH'
set -e
git config --global --add safe.directory '*'
rev="$(git rev-parse HEAD 2>/dev/null || true)"
if [ -n "$REVISION" ] && [ -n "$rev" ] && [ "$REVISION" != "$rev" ]; then echo "REVISION=$REVISION but context is $rev" >&2; exit 1; fi
rev="${REVISION:-$rev}"
if [ -z "$rev" ]; then echo "no .git in context: pass --build-arg REVISION=<sha>" >&2; exit 1; fi
PKG=github.com/moby/buildkit
GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
  -ldflags "-X ${PKG}/version.Version=${RELEASE_VERSION} -X ${PKG}/version.Revision=${rev} -X ${PKG}/version.Package=github.com/EarthBuild/buildkit -extldflags '-static'" \
  -tags "osusergo netgo static_build seccomp" -o /out/buildkitd ./cmd/buildkitd
SH

# Only the daemon binary changes; runc, buildctl, entrypoint and config stay stock.
FROM ${BASE_IMAGE}
COPY --link --from=buildkitd /out/buildkitd /usr/bin/buildkitd
ARG REVISION
LABEL org.opencontainers.image.source="https://github.com/bastaai/buildkit" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.description="earthbuild/buildkitd:v0.8.18-b0385986 with a patched buildkitd (moby/buildkit#7268)"
