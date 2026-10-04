# rad-actions: Radicle CI for IPCR Forge. Three upstream pieces and one of ours:
#   cib        radicle-ci-broker, unmodified — follows the node's events, filters them, runs the adapter,
#              writes HTML reports and job COBs
#   act        nektos/act, unmodified — runs GitHub Actions workflows against a Docker daemon
#   git        checks the commit out of the node's storage
#   rad-actions (adapter/) — maps a Radicle event onto a GitHub event and runs act
# Build context: this folder (ci/).
ARG RUST_VERSION=1.90
FROM rust:${RUST_VERSION}-bookworm AS cib
ARG CIB_VERSION=0.32.1
RUN cargo install radicle-ci-broker --version ${CIB_VERSION} --locked --root /out

FROM golang:1.24-alpine AS adapter
WORKDIR /src
COPY adapter/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/rad-actions .

FROM debian:bookworm-slim
ARG ACT_VERSION=0.2.89
ARG TARGETARCH=amd64
ARG VERSION=dev
LABEL org.opencontainers.image.title="rad-actions" \
      org.opencontainers.image.description="Radicle CI broker + act: GitHub-style workflows for Radicle repositories" \
      org.opencontainers.image.source="https://github.com/Yundera/ipcr" \
      org.opencontainers.image.version="$VERSION"
# Retried: GitHub release downloads intermittently answer 503.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/* \
 && arch=$([ "$TARGETARCH" = arm64 ] && echo arm64 || echo x86_64) \
 && url="https://github.com/nektos/act/releases/download/v${ACT_VERSION}/act_Linux_${arch}.tar.gz" \
 && for i in 1 2 3 4 5; do curl -fsSL -o /tmp/act.tgz "$url" && break; sleep $((i * 5)); done \
 && tar -xzf /tmp/act.tgz -C /usr/local/bin act && rm /tmp/act.tgz
COPY --from=cib /out/bin/ /usr/local/bin/
COPY --from=adapter /out/rad-actions /usr/local/bin/rad-actions
COPY ci-broker.yaml /etc/rad-actions/ci-broker.yaml
# Checkouts fetch from the node's storage, owned by the node's uid; git refuses to serve a repository
# it does not own ("dubious ownership") whenever the uids differ.
RUN git config --system safe.directory '*'
ENV RAD_HOME=/radicle-home \
    RAD_ACTIONS_LOG_DIR=/ci/html/logs \
    RAD_ACTIONS_WORK_DIR=/ci/work \
    RAD_ACTIONS_CACHE_DIR=/ci/cache/act \
    HOME=/ci/home
# info: the default (trace) logs every gossip message the node sees.
CMD ["cib", "--config", "/etc/rad-actions/ci-broker.yaml", "--log-level", "info", "process-events"]
