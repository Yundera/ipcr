# ipcr-forge-bridge: the Gitea → Radicle mirror and IPCR Forge's page. Our Go binary, plus git and
# the stock Radicle CLI (`rad`, `git-remote-rad`) at the version the forge's node runs.
# Build context: this folder (bridge/).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ipcr-forge-bridge .

# Radicle's official static (musl) release, checked against its published digest.
FROM alpine:3.22 AS radicle
ARG RADICLE_VERSION=1.10.1
ARG RADICLE_SHA256=9354f7f6241a88ddca1d41a080aa6337ddfc5dc682b8de78ab6d418b5782225a
# Retried: release downloads intermittently fail.
RUN apk add --no-cache xz \
 && url="https://files.radicle.dev/releases/${RADICLE_VERSION}/radicle-${RADICLE_VERSION}-x86_64-unknown-linux-musl.tar.xz" \
 && for i in 1 2 3 4 5; do wget -qO /tmp/radicle.tar.xz "$url" && break; sleep $((i * 5)); done \
 && echo "${RADICLE_SHA256}  /tmp/radicle.tar.xz" | sha256sum -c - \
 && mkdir /out && tar -xJf /tmp/radicle.tar.xz -C /out --strip-components=2 \
      "radicle-${RADICLE_VERSION}-x86_64-unknown-linux-musl/bin/rad" \
      "radicle-${RADICLE_VERSION}-x86_64-unknown-linux-musl/bin/git-remote-rad"

FROM alpine:3.22
ARG VERSION=dev
LABEL org.opencontainers.image.title="ipcr-forge-bridge" \
      org.opencontainers.image.description="IPCR Forge: mirrors public Gitea repositories to Radicle, serves the forge's page" \
      org.opencontainers.image.source="https://github.com/Yundera/ipcr" \
      org.opencontainers.image.version="$VERSION"
RUN apk add --no-cache ca-certificates git \
 # Mirrors and the node's storage belong to the service's uid, but a bind mount can make git see
 # another owner ("dubious ownership"); the storage is the node's own, so trust it.
 && git config --system safe.directory '*'
COPY --from=radicle /out/ /usr/local/bin/
COPY --from=build /out/ipcr-forge-bridge /usr/local/bin/ipcr-forge-bridge
ENV RAD_HOME=/radicle-home \
    HOME=/tmp
# No entrypoint: the install step runs `sh -c '… ipcr-forge-bridge setup'` in this image.
CMD ["ipcr-forge-bridge", "serve"]
