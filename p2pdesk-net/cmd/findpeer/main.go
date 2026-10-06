// findpeer: standalone DHT lookup tool — bootstraps, waits for the routing
// table, then FindPeer's the target and prints the returned addresses.
// Usage: findpeer [-bootstrap <comma-separated multiaddrs>] [-protocol <prefix>] <peer-id>
// Without flags it queries the public IPFS DHT (the original behavior);
// with flags it joins a custom network (same values as the Custom Network
// setting / bootstrapd output line).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"

	"p2pdesk-net/nodekit"
)

func main() {
	bootstrap := flag.String("bootstrap", "", "comma-separated bootstrap multiaddrs (with /p2p/); empty = public IPFS DHT")
	protocol := flag.String("protocol", "", "DHT protocol prefix; empty = /ipfs (public DHT)")
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("usage: findpeer [-bootstrap <multiaddrs>] [-protocol <prefix>] <peer-id>")
		os.Exit(2)
	}
	target, err := peer.Decode(args[0])
	if err != nil {
		fmt.Println("bad peer id:", err)
		os.Exit(2)
	}
	if err := nodekit.ValidateProtocolPrefix(*protocol); err != nil {
		fmt.Println("bad protocol:", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	priv, _, _ := crypto.GenerateEd25519Key(nil)
	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.NoListenAddrs,
		libp2p.DefaultTransports,
	)
	if err != nil {
		fmt.Println("host:", err)
		os.Exit(1)
	}
	defer h.Close()

	var bsPeers *[]string
	if strings.TrimSpace(*bootstrap) != "" {
		list := strings.FieldsFunc(*bootstrap, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
		})
		bsPeers = &list
	}
	d, err := nodekit.NewDHT(ctx, h, nodekit.DHTConfig{
		BootstrapPeers: bsPeers,
		ProtocolPrefix: *protocol,
	})
	if err != nil {
		fmt.Println("dht:", err)
		os.Exit(1)
	}

	// Wait for the routing table to have peers.
	deadline := time.Now().Add(90 * time.Second)
	for d.RoutingTable().Size() == 0 && time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
	}
	fmt.Printf("routing table size: %d\n", d.RoutingTable().Size())

	start := time.Now()
	info, err := d.FindPeer(ctx, target)
	fmt.Printf("FindPeer took %s\n", time.Since(start))
	if err != nil {
		fmt.Println("FindPeer error:", err)
		os.Exit(1)
	}
	fmt.Printf("found %s: %v\n", info.ID.ShortString(), info.Addrs)
}
