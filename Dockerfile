# One image, three roles (see bin/entrypoint): the IPCR gateway (ipcrd), the stock nerdctl IPFS
# registry it fronts, and the `ipcr` publish CLI.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY gateway/ .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /ipcrd .

FROM alpine:3.22
ARG NERDCTL_VERSION=2.4.1
ARG TARGETARCH
ARG VERSION=dev
LABEL org.opencontainers.image.title="ipcr" \
      org.opencontainers.image.description="IPCR — pull container images from IPFS with vanilla docker: ipcr.localhost:4767/ipfs/<cid>" \
      org.opencontainers.image.source="https://github.com/Yundera/ipcr" \
      org.opencontainers.image.version="$VERSION"
# Retried: GitHub release downloads intermittently answer 503.
RUN apk add --no-cache ca-certificates \
 && url="https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH}.tar.gz" \
 && for i in 1 2 3 4 5; do wget -qO /tmp/nerdctl.tgz "$url" && break; sleep $((i * 5)); done \
 && tar -xzf /tmp/nerdctl.tgz -C /usr/local/bin nerdctl && rm /tmp/nerdctl.tgz
COPY --from=build /ipcrd /usr/local/bin/ipcrd
COPY --chmod=0755 bin/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/entrypoint"]
CMD ["gateway"]
