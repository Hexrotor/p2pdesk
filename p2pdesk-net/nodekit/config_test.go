package nodekit

import (
	"strings"
	"testing"
)

func TestValidateProtocolPrefix(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		wantErr bool
	}{
		{"empty is the public DHT", "", false},
		{"valid", "/p2pdesk", false},
		{"nested segments ok", "/p2pdesk/private", false},
		{"missing leading slash", "p2pdesk", true},
		{"whitespace rejected", "/p2p desk", true},
		{"too long", "/" + strings.Repeat("a", 200), true},
		{"bare root rejected", "/", true},
		{"doubled slash rejected", "/p2p//desk", true},
		{"full protocol string rejected", "/p2pdesk/kad/1.0.0", true},
		{"kad-ish suffix rejected", "/p2pdesk/kad", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateProtocolPrefix(c.prefix)
			if (err != nil) != c.wantErr {
				t.Fatalf("ValidateProtocolPrefix(%q) = %v, wantErr %v", c.prefix, err, c.wantErr)
			}
		})
	}
}

func TestParseConfigBootstrapThreeState(t *testing.T) {
	// nil (absent) stays nil: public DHT.
	cfg, err := ParseConfig(`{"identity_dir": "/tmp/x", "mode": "client"}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapPeers != nil {
		t.Fatalf("absent bootstrap_peers should decode to nil, got %v", *cfg.BootstrapPeers)
	}

	// Empty list decodes to a non-nil empty slice: root node.
	cfg, err = ParseConfig(`{"identity_dir": "/tmp/x", "bootstrap_peers": []}`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapPeers == nil || len(*cfg.BootstrapPeers) != 0 {
		t.Fatalf("empty bootstrap_peers should decode to non-nil empty slice, got %#v", cfg.BootstrapPeers)
	}

	// Missing identity_dir is fatal.
	if _, err := ParseConfig(`{"mode": "server"}`); err == nil {
		t.Fatal("missing identity_dir should fail")
	}

	// Invalid prefix is fatal at parse time.
	if _, err := ParseConfig(`{"identity_dir": "/tmp/x", "protocol_prefix": "nope"}`); err == nil {
		t.Fatal("invalid protocol_prefix should fail")
	}
}

func TestParseBootstrapPeers(t *testing.T) {
	valid := "/ip4/1.2.3.4/tcp/28432/p2p/12D3KooWFNkdmdb38vCKEbRSMKQkDcW2Q7FZ7mL2Y3RcKU9BYj1e"
	infos, bad := ParseBootstrapPeers([]string{valid, "garbage", "", "/ip4/1.2.3.4/tcp/28432"})
	if len(infos) != 1 || len(bad) != 2 {
		t.Fatalf("got %d infos %d bad, want 1/2", len(infos), len(bad))
	}
}
