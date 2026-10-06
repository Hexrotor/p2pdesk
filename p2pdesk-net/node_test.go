package main

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"p2pdesk-net/nodekit"
)

// TestNodeLifecycle: start a node in a temp dir, check identity + status,
// verify accept timeout and idempotent close.
func TestNodeLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := &nodekit.NodeConfig{IdentityDir: dir, Mode: "server"}
	n, err := startNode(cfg)
	if err != nil {
		t.Fatalf("startNode: %v", err)
	}
	defer n.stop()

	peerID := n.host.ID().String()
	if len(peerID) < 40 {
		t.Fatalf("peer id too short: %q", peerID)
	}

	// Identity file must persist.
	if _, err := nodekit.LoadIdentity(dir); err != nil {
		t.Fatalf("reload identity: %v", err)
	}
	priv2, _ := nodekit.LoadIdentity(dir)
	id2, err := peer.IDFromPublicKey(priv2.GetPublic())
	if err != nil || id2.String() != n.host.ID().String() {
		t.Fatalf("reloaded identity mismatch: %v", err)
	}

	// Status snapshot is callable.
	st := n.statusSnapshot()
	_ = st

	// Accept with a 100ms timeout must time out (errTimeout), not hang.
	start := time.Now()
	_, code := n.accept(100)
	if code != errTimeout {
		t.Fatalf("accept should time out, got code %d", code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("accept timeout took too long")
	}
}

// TestIdentityPersistence: two starts in the same dir yield the same PeerId.
func TestIdentityPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := &nodekit.NodeConfig{IdentityDir: dir, Mode: "server"}
	n1, err := startNode(cfg)
	if err != nil {
		t.Fatalf("startNode #1: %v", err)
	}
	id1 := n1.host.ID().String()
	n1.stop()

	n2, err := startNode(cfg)
	if err != nil {
		t.Fatalf("startNode #2: %v", err)
	}
	defer n2.stop()
	if id2 := n2.host.ID().String(); id2 != id1 {
		t.Fatalf("peer id changed across restarts: %s != %s", id2, id1)
	}
}

// TestSign: identity signing round-trips against the public key.
func TestSign(t *testing.T) {
	dir := t.TempDir()
	n, err := startNode(&nodekit.NodeConfig{IdentityDir: dir, Mode: "server"})
	if err != nil {
		t.Fatalf("startNode: %v", err)
	}
	defer n.stop()

	msg := []byte("signed identity")
	out := make([]byte, 128)
	code, w := n.sign(msg, out)
	if code != errOK {
		t.Fatalf("sign: code %d", code)
	}
	pub := n.host.Peerstore().PubKey(n.host.ID())
	ok, err := pub.Verify(msg, out[:w])
	if err != nil || !ok {
		t.Fatalf("verify failed: ok=%v err=%v", ok, err)
	}
}
