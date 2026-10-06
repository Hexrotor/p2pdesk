# p2pdesk Security Statement

This document describes the security posture of p2pdesk: what the peer-to-peer
design protects, what it deliberately does not, and the known attack surface
with its current mitigations. Read it before exposing a p2pdesk node to the
internet.

## Threat model in one paragraph

p2pdesk replaces the hbbs/hbbr rendezvous + relay servers with a go-libp2p
node on every device. Peer identities are self-registered ed25519 keypairs:
any device can mint any PeerId, and there is no certificate authority or
central registry. The PeerId is public on the DHT — anyone can look it up,
dial the host, and reach the login handshake. **The session password (or the
one-time password) is the security baseline that gates control.** Everything
else below is about keeping the door handle from being turned, not about
replacing the lock.

## Protections in place

- **Session data never traverses a relay.** A relay may only carry the
  short-lived hole-punch coordination (DCUtR or the custom punch signaling).
  If neither direct path is established, the connection fails instead of
  downgrading to relay transport.
- **PeerId-bound authentication.** On every p2p connection the target signs a
  `SignedId` with the same ed25519 key that backs its PeerId; the controller
  verifies the signature against the PeerId itself. A peer claiming an
  identity it does not hold the key for is rejected at the handshake.
- **The identity keypair never leaves the network module.** Rust only sees
  signatures; the Go module keeps the private key in memory.
- **Connection budget.** The libp2p connection manager caps the node at a
  small connection budget (20 low-water / 40 high-water), so a flood of
  incoming dials cannot hold the host open indefinitely.
- **Resource quotas.** The libp2p ResourceManager runs with auto-scaled
  default limits, capping streams, file descriptors and memory per peer and
  per protocol on top of the connection budget.
- **Hole-punch rate limiting.** A failed punch blacklists the peer for an
  hour; spray rounds have a hard packet budget with a shrinking retry
  schedule, so a malicious peer cannot drive unbounded traffic out of the
  node.
- **Node-wide punch rate limiting.** Every inbound punch session draws from
  one global token bucket (burst 3, one refill per 30 s) shared by all
  peers. Because PeerIds are mintable for free, per-peer cooldowns cannot
  stop identity rotation; the global bucket caps responder spray at ~1.5 MB
  burst and ~1 MB/min sustained, no matter how many identities ask. The
  bucket is a damage cap, not access control.
- **Punched QUIC sessions are token-authenticated.** The QUIC handshake after
  a punch carries a session token exchanged over the signaling stream; a
  third party that merely observes the spray cannot hijack the session.
- **Optional whitelist.** The `peer-whitelist` option restricts inbound p2p
  sessions to a list of PeerIds.
- **Numeric-ID leftover removed.** Inbound p2p connections display the
  peer's real PeerId (bound to the transport handshake), not the hbbs-era
  9-digit id, which carried no meaning without a registration server.

## Known attack surface and current mitigations

| Surface | What an attacker can do | Current mitigation |
|---|---|---|
| Public DHT | Discover any node's PeerId, addresses, and online presence | Inherent to the design — treat the PeerId as a public username |
| Pre-auth dial | Dial the host's QUIC/TCP listener from anywhere; the login handshake runs before any authorization | Connection-manager budget; handshake rejection is cheap; session password gates control |
| Punch-spray reflection | Open a punch signaling stream and claim an arbitrary `PublicIP`, inducing the responder to spray UDP packets at a third-party address | Per-peer 1h cooldown after failure; hard spray packet budget; QUIC token prevents session hijack; the node-wide punch bucket bounds the total spray rate that can be redirected at a third party |
| Password guessing | Repeated login attempts | The stock rustdesk password verification with its error delay; one-time password mode; a `peer-whitelist` for locked-down deployments |
| DHT noise | Every node participates in DHT maintenance traffic | Typical traffic is on the order of 30 KB/s; the DHT protocol's own limits apply |
| Relay abuse | A malicious relay could observe hole-punch coordination metadata (who coordinates with whom, at what mapped addresses) | Relays never see session data, keys, passwords, or the punched QUIC session; the relay set is the public libp2p relay pool or a custom-network's own |
| Sybil punch flood | PeerIds are mintable for free, so the per-peer punch cooldown can be bypassed by rotating identities: each new identity can drive a full spray round out of the node (~0.5 MB of UDP per session) | Node-wide token bucket (burst 3, one refill per 30 s) gates every punch session regardless of identity; refusals cost the responder one small frame and fail the initiator fast |

## Honest limitations (not mitigated, by nature or by design)

- **The 9-digit numeric id is guessable.** The rustdesk protocol still
  exchanges a randomly generated 9-digit id during login. A 10^9 space is
  enumerable. p2pdesk does not use it as an identity (the PeerId is) nor as
  an authentication factor (the password is); it is a protocol remnant with
  no security value in either direction. This weakness is inherited from
  upstream rustdesk.
- **Network-level floods (SYN/UDP floods against the listen port) are out of
  scope.** They operate below the application layer; any libp2p node with a
  public listener is equally exposed. OS-level firewall rules are the
  appropriate mitigation.
- **Idle connection squatting.** A peer can dial and then do nothing,
  occupying a connection-manager slot until the idle grace period evicts
  it. A Sybil attacker could hold the 40-connection budget busy and delay
  legitimate sessions; the budget itself keeps this bounded and the login
  handshake timeout reclaims slots within seconds.

## Deliberate limitations

- **Hard-symmetric NATs.** Devices behind symmetric-hard NATs (typical of
  carrier-grade mobile NAT) rarely establish a direct path, and p2pdesk has
  no relay fallback for session data. The connection fails rather than
  silently routing data through a relay. LAN or predictable-NAT networks are
  the reliable deployment targets.
- **No trust root.** A PeerId is only as trustworthy as the secret that
  backs it. Verify a peer's identity out of band (e.g. a QR code scanned in
  person) before connecting to it the first time.
- **DHT privacy.** The DHT records a node's participation; IP addresses
  announced to the DHT are filtered to public addresses, but presence and
  PeerId are public information.

## Planned hardening

- **Return-routability challenge (analyzed, deferred).** The natural fix
  for spray reflection — the responder sends a random token to the
  initiator's claimed `PublicIP:PublicPort` before spraying and requires it
  back over signaling — only works when that address actually routes to the
  initiator. A symmetric NAT does not forward such packets: the claimed
  mapping is the initiator↔STUN mapping, and the token arrives from the
  responder's address instead. A challenge applied uniformly would break
  the sym-to-cone and easy-sym paths that carrier-NAT devices depend on,
  and the alternative that sends the token to the source address of the
  first received spray packet reintroduces a priming spray of its own.
  Until a variant is designed that preserves symmetric-NAT support, the
  node-wide punch bucket is the standing mitigation: an attacker claiming a
  victim's address can redirect at most the ~1.5 MB burst / ~1 MB/min
  sustained ceiling, and every such attempt consumes the attacker's own
  share of the same bucket.
