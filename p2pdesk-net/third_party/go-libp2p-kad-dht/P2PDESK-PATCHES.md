# P2PDesk endpoint maintenance patch

Base: `github.com/libp2p/go-libp2p-kad-dht v0.39.2`, copied from the verified Go module cache. Original license files remain in this directory. The parent `p2pdesk-net/go.mod` uses a local `replace`; normal Windows and Android c-shared builds both compile this source. No module-cache edits or network protocol changes are required.

Local changes:

* `RoutingTableCheckConcurrency(n)` passes an optional limit to the refresh manager. P2PDesk endpoints choose 16; callers without the option retain the upstream behavior. Acquire a worker **before** starting the peer's ten-second timeout; cancellation does not evict otherwise healthy queued/active peers.
* `internal/net/message_manager.go` protects peers for the duration of an outbound RPC. Unique tags cover concurrent callers and are always released, including errors. Cached idle streams remain eligible for normal connection-manager collection.
* Query and liveness workers also protect connection/identify setup. Maintenance queries use their child cancellation context to stop surplus RPCs after normal alpha/beta convergence; cancellation does not evict these peers.
* Opt-in `RoutingTableRefreshWithoutFollowup` skips the publication-oriented top-K followup for background bucket maintenance. Public `GetClosestPeers`, `FindPeer`, and provider/value operations retain upstream semantics. `refresh_lookup_test.go` verifies fewer maintenance RPCs, unchanged public followups, and prompt cancellation of surplus slow queries without evictions.
* Refresh-manager lifecycle messages use go-libp2p's slog bridge so they reach the existing Rust log callback. Only this logger's lifecycle messages are admitted at normal verbosity by P2PDesk.
* `rtrefresh/bounded_check_test.go` tests concurrency, queueing longer than a peer timeout, and shutdown without false evictions. Existing upstream tests are retained.

Keep the upstream base version and these changes explicit when updating the dependency. Run `go test ./rtrefresh ./internal/net` here and the parent module tests after rebasing.
