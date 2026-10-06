# p2pdesk

Serverless peer-to-peer remote desktop — a [rustdesk](https://github.com/rustdesk/rustdesk) 1.4.9 fork
where the hbbs/hbbr rendezvous and relay servers are replaced by a go-libp2p c-shared module
(`p2pdesk-net/`, loaded at runtime via `src/p2pffi.rs`). Modified by Hexrotor, released under AGPL-3.0.

No account, no self-hosted server: the controller finds the target by its libp2p PeerId on the public
IPFS DHT and connects directly. Data never traverses a relay — a relay may only carry the short-lived
DCUtR hole-punch coordination that libp2p performs on its own.

Maintained on a best-effort basis as a personal fork. Read "Known limitations" before relying on it
for unattended access.

## Documentation

- [docs/BUILD.md](docs/BUILD.md) — building the Windows target, the Android
  controller APK, the Go module, and the packaging scripts.
- [docs/NETWORK.md](docs/NETWORK.md) — running your own network: a self-hosted
  DHT bootstrap node, protocol isolation, and the relay role.
- [docs/SECURITY-STATEMENT.md](docs/SECURITY-STATEMENT.md) — threat model,
  implemented mitigations, and the gaps that are known and accepted.
- [docs/SECURITY.md](docs/SECURITY.md) — how to report a vulnerability.

## How it works

- Both sides run a go-libp2p node. The target also enables relay + AutoRelay so hole punching can
  coordinate through public relays; both sides register themselves on the IPFS DHT.
- Connect = `FindPeer(PeerId)` → dial (TCP/QUIC, multi-address) → DCUtR direct upgrade → open a control
  stream → the stock rustdesk handshake runs on top: the target signs a `SignedId` with the same ed25519
  key that backs its PeerId, the controller verifies the signature against the PeerId itself (no hbbs
  public key), then password login proceeds unchanged.
- The connection password stays the security baseline — PeerIds on a public DHT are discoverable, the
  password is what gates control.
- The identity keypair never leaves the Go module (`p2pd_node_sign`); Rust only ever sees signatures.

## Building

See [docs/BUILD.md](docs/BUILD.md).

Short version:

- **Windows target**: `python res/inline-sciter.py`, build the Go DLL in `p2pdesk-net/`
  (`-buildmode=c-shared`), then `cargo build --release --features inline`. Deploy three files side by
  side: `p2pdesk.exe`, `sciter.dll`, `p2pdesk_net.dll`.
- **Android controller APK**: cross-build the Go `.so` for arm64, run flutter_rust_bridge codegen
  (the generated files are gitignored), `bash scripts/build-android.sh --lib`, copy both `.so`
  files into `jniLibs`, then `flutter build apk` (Flutter **3.24.5** pinned).

## Operational notes

- The target's PeerId comes from the persistent p2p identity — machine-wide on Windows
  (`%ProgramData%\P2PDesk\config\identity.key`), shared by the service and portable builds.
  Deleting it mints a new PeerId on the next start.
- Go-side logs are bridged into the rustdesk log (`[go]` prefix). libp2p-internal churn (identify,
  DHT gossip) is capped at Error; set `P2PDESK_GO_LOG=debug` for full verbosity.
- `p2pdesk-connect <peer_id> [password]` is a headless connectivity/link check that prints stage
  timings and ends with a machine-readable `BENCH_RESULT` line.

## Known limitations

- **No data relay fallback**: if direct connectivity fails (e.g. both sides behind symmetric NAT),
  the session cannot be established. Custom hole punching is not implemented yet.
- **Locked/sleeping targets**: a portable user-level process cannot capture the Winlogon desktop or
  inject input into it, and a sleeping monitor produces no frames. For unattended targets run the
  target as a Windows service with an indirect display driver (IDD) — not wired up in this fork yet.
- Discovery depends on reachability of the public IPFS bootstrap nodes.

## Not planned

Explicitly out of scope for this fork; not expected to be implemented:

- **Android as the controlled side**: the Android build is controller-only. Capturing
  and serving the Android screen (media projection + foreground service) is not wired up.
- **Formal performance benchmarking**: only the headless `p2pdesk-connect` link check
  exists. No systematic throughput/latency comparison of direct vs hole-punched vs
  relay-coordinated paths has been produced.
- **Custom data-relay fallback**: direct connectivity is required; see "Known limitations".

## License

AGPL-3.0, inherited from rustdesk. Original rustdesk copyright notices remain in the source files.
