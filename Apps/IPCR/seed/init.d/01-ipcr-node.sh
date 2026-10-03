#!/bin/sh
# Run by Kubo's entrypoint before every daemon start (after `ipfs init` on the first one),
# so a store update that changes these values reaches an installed node.
set -e

# libp2p swarm on 4768 rather than Kubo's 4001: the Kubo store app already publishes 4001 and
# two apps cannot publish the same host port. Listening on the published port itself (instead of
# mapping 4768→4001) keeps the addresses the node announces dialable.
P=${IPCR_SWARM_PORT:-4768}
ipfs config --json Addresses.Swarm "[
  \"/ip4/0.0.0.0/tcp/$P\", \"/ip6/::/tcp/$P\",
  \"/ip4/0.0.0.0/udp/$P/quic-v1\", \"/ip6/::/udp/$P/quic-v1\",
  \"/ip4/0.0.0.0/udp/$P/quic-v1/webtransport\", \"/ip6/::/udp/$P/quic-v1/webtransport\",
  \"/ip4/0.0.0.0/udp/$P/webrtc-direct\", \"/ip6/::/udp/$P/webrtc-direct\"
]"

# Soft cap on the block store. Pinned images are never collected; with --enable-gc everything
# else (blocks fetched but not pinned) is swept every Datastore.GCPeriod.
ipfs config Datastore.StorageMax "${IPFS_STORAGE_MAX:-20GB}"
