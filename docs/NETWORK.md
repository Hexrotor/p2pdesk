# Self-hosted network (custom DHT)

By default p2pdesk devices discover each other on the public IPFS DHT. A
self-hosted network replaces that with your own DHT seeded by a `bootstrapd`
root node, and isolates it at the protocol level so it never exchanges peers
with the public network (and vice versa).

## The two settings

Both live under **Settings → Custom Network** on the desktop (menu) and the
mobile app (Settings screen), and both are empty by default:

| Setting | Meaning |
|---|---|
| Bootstrap peers | One multiaddr per line, as printed by `bootstrapd` (see below). Empty = join the public IPFS DHT. |
| Protocol | The DHT protocol prefix every device in the network must share, e.g. `/p2pdesk`. Empty = the public `/ipfs` prefix. |

Rules:

- **All devices must use the same protocol string.** A device with a
  different (or empty) protocol joins a different DHT and will never see the
  rest of the network — this is the isolation, not just a filter.
- Filling only the peers leaves the protocol at the `bootstrapd` default
  (`/p2pdesk`); empty protocol with empty peers = the public DHT, exactly the
  pre-custom-network behavior.
- Filling only the protocol is ignored: without at least one valid peer the
  device stays on the public DHT (a protocol-only config would dial the
  public bootstrap list while being invisible to it).
- Changes take effect on restart (the p2p module initializes once per
  process).
- A list of peers that are all invalid fails node startup loudly, so a typo
  cannot silently drop a device back onto the public DHT.

The persistent config lives in `config2.toml` under `[options]`:
`custom-bootstrap-peers` (comma-separated) and `custom-bootstrap-protocol`.
For debugging/acceptance runs, the env vars `P2PDESK_BOOTSTRAP_PEERS` and
`P2PDESK_PROTOCOL_PREFIX` work without touching the config file (config wins
when both are set).

## Running the root node

```bash
bootstrapd -port 28432 -identity /var/lib/p2pdesk-bootstrap \
    -external-addr /ip4/PUBLIC_IP/tcp/28432 -protocol /p2pdesk
```

- `-identity` holds the ed25519 key; keep the directory persistent or the
  network's PeerId changes on every restart.
- `-external-addr` is the address remote devices can actually dial (one
  multiaddr per listener you forward; no `/p2p/` component — it is appended).
- The node dials **no** bootstrap peers of its own, so the DHT it seeds never
  touches the public network.
- The port must accept **TCP and UDP** from the internet (see Firewall
  below).

Startup prints the PeerId and one paste-ready line, e.g.:

```
/ip4/1.2.3.4/tcp/28432/p2p/12D3KooW...,/ip4/1.2.3.4/udp/28432/quic-v1/p2p/12D3KooW...
```

Copy that line into the Bootstrap peers field of every device (the extra
local-address entries are for LAN debugging and are harmless on WAN
clients).

Example systemd unit:

```ini
[Unit]
Description=p2pdesk bootstrap node
After=network-online.target

[Service]
ExecStart=/usr/local/bin/bootstrapd -port 28432 -identity /var/lib/p2pdesk-bootstrap -external-addr /ip4/1.2.3.4/tcp/28432 -protocol /p2pdesk
Restart=always
User=p2pdesk
RuntimeDirectory=p2pdesk-bootstrap
RuntimeDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

## Firewall

Open the `-port` (default 28432) for both TCP and UDP: TCP carries the DHT
and the relay transport, UDP the QUIC transport. `-external-addr` must match
the address as seen from the outside, so NAT setups need a port forward and a
public IP.

## Redundancy

The bootstrap is a single point in the network's *join path* only — once
devices have each other in their routing tables they keep finding each other,
and hole-punched connections are device-to-device. Still, run two bootstrap
nodes for availability: point each at the other with `-bootstrap`:

```bash
# node A (identity dir A)
bootstrapd -port 28432 -external-addr /ip4/IP_A/tcp/28432 \
    -bootstrap /ip4/IP_B/tcp/28432/p2p/PEER_B
# node B (identity dir B)
bootstrapd -port 28432 -external-addr /ip4/IP_B/tcp/28432 \
    -bootstrap /ip4/IP_A/tcp/28432/p2p/PEER_A
```

Devices list both nodes in their Bootstrap peers field; the DHT retries the
list periodically, so one node being down only delays the first discovery
after a cold start.

## The relay role

`bootstrapd` doubles as a circuitv2 relay (`-relay true`, the default).
That is the network's **signaling path**: the hole-punching handshake and
fallback signaling ride relay streams, and the DHT auto-relay discovery uses
it. Session data never traverses relays — the connect path refuses relay
hops for the media connection, so the relay only ever carries signaling
volume. Limits are tuned for a small network (256 reservations in total,
32 per IP, 64 per ASN, 1h TTL); tighten them in
`p2pdesk-net/cmd/bootstrapd/main.go` for larger deployments, or run a
dedicated relay host and start bootstrapd with `-relay false` (in that case
the dedicated relay must be reachable and advertised to the DHT so devices
can reserve slots on it).

## Isolation model

- Every node speaks its DHT under `/PREFIX/kad/1.0.0` (e.g.
  `/p2pdesk/kad/1.0.0`). A node only keeps peers that advertise the same
  protocol in their routing table, and refuses queries from other protocols.
  The isolation holds in both directions: public-DHT nodes cannot enter a
  `/p2pdesk` table and `/p2pdesk` nodes cannot enter a public table.
- The root node dials nothing on startup (empty bootstrap list), so a
  self-hosted network is not seeded from the public DHT either.
- LAN discovery (UDP broadcast) bypasses the DHT entirely and still works
  inside a custom network.
- libp2p still exchanges addresses and peer IDs with anything that dials a
  node directly (identify); the DHT protocol prefix is not a firewall. The
  session layer's PeerId signature check remains the actual authentication
  for connections.

## Troubleshooting

- **Devices never find each other after switching**: check the protocol
  string is identical on every device (trailing slashes are stripped by the
  UI, but case matters). The bootstrapd startup line prints the protocol it
  runs.
- **Nothing happens on a phone**: confirm the paste line uses the public
  `-external-addr`, not a local one, and the port is reachable (`nc -vz` TCP
  + a UDP probe).
- **Both empty in the UI**: that is the public DHT, not an error.
- **Node fails to start with a peers error**: every listed multiaddr was
  invalid (missing `/p2p/` PeerId component, for example).
