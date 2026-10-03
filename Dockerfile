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
RUN apk add --no-cache ca-certificates \
 && wget -qO- "https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH}.tar.gz" \
    | tar -xz -C /usr/local/bin nerdctl
COPY --from=build /ipcrd /usr/local/bin/ipcrd
COPY bin/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/entrypoint"]
CMD ["gateway"]
