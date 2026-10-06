// Package nodekit holds the host/DHT/identity construction shared by the
// DLL (package main) and the standalone tools (cmd/bootstrapd,
// cmd/findpeer). Keeping one source of truth prevents the host option set
// from drifting between binaries — a mismatch there surfaces as peers that
// dial fine but fail protocol negotiation, which is hard to diagnose.
package nodekit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
)

// NodeConfig is the JSON handed to p2pd_node_start by the Rust side.
type NodeConfig struct {
	IdentityDir  string `json:"identity_dir"`
	Mode         string `json:"mode"` // "server" | "client"
	ExternalAddr string `json:"external_addr,omitempty"`
	Port         int    `json:"port,omitempty"` // fixed listen port (LAN discovery); 0 = 28431

	// Custom network (self-hosted DHT). ABI v1 compatible: unknown fields
	// were silently ignored by older modules, and the Rust side defaults
	// both when absent.
	//
	// BootstrapPeers is three-state:
	//   nil          = not configured -> public IPFS DHT (unchanged default)
	//   []string{}   = root node: dial no bootstrap peers at all
	//   [list]       = custom bootstrap peers
	BootstrapPeers *[]string `json:"bootstrap_peers,omitempty"`
	// ProtocolPrefix names the DHT protocol namespace; empty = /ipfs.
	// Non-empty gives /<prefix>/kad/1.0.0, which is invisible to the
	// public IPFS DHT and vice versa.
	ProtocolPrefix string `json:"protocol_prefix,omitempty"`
}

// maxBootstrapPeers caps how many custom bootstrap entries are accepted.
// fixLowPeers dials at most 2 per cycle anyway, so anything beyond this is
// a misconfiguration (or a paste accident) and is skipped with a warning.
const maxBootstrapPeers = 32

// ParseConfig decodes the JSON handed by p2pd_node_start.
func ParseConfig(configJSON string) (*NodeConfig, error) {
	var cfg NodeConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.IdentityDir == "" {
		return nil, fmt.Errorf("config missing identity_dir")
	}
	if cfg.Mode == "" {
		cfg.Mode = "server"
	}
	if err := ValidateProtocolPrefix(cfg.ProtocolPrefix); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ValidateProtocolPrefix checks a configured DHT protocol prefix. A bad
// prefix is a hard error (not a warning): it fails silently at runtime as
// "nobody can find anybody", which is far harder to diagnose than a
// refusal to start. The UI normalizes (leading /, no trailing /) before
// saving, so a failure here means a hand-edited config file.
func ValidateProtocolPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("protocol_prefix must start with '/': %q", prefix)
	}
	if strings.ContainsAny(prefix, " \t\r\n") {
		return fmt.Errorf("protocol_prefix must not contain whitespace: %q", prefix)
	}
	if len(prefix) > 128 {
		return fmt.Errorf("protocol_prefix too long (>128): %q", prefix)
	}
	// Common mis-pastes that would silently mint a wrong-but-valid
	// namespace: the bare root, doubled slashes, or the full protocol
	// string (which gets "/kad/1.0.0" appended on top of its own suffix).
	if prefix == "/" {
		return fmt.Errorf("protocol_prefix must name a namespace, not just '/': %q", prefix)
	}
	if strings.Contains(prefix, "//") {
		return fmt.Errorf("protocol_prefix must not contain '//': %q", prefix)
	}
	if strings.Contains(prefix, "/kad") {
		return fmt.Errorf("protocol_prefix must be the namespace only, without the '/kad/...' suffix: %q", prefix)
	}
	return nil
}

// ParseBootstrapPeers parses a configured bootstrap list. It returns the
// valid entries plus the rejected lines. Partial failure skips bad lines
// with a warning; the caller decides whether "all bad" is fatal — for a
// node that was explicitly configured for a custom network, silently
// falling back to the public DHT would be a privacy hazard, so NewDHT
// treats it as an error.
func ParseBootstrapPeers(list []string) ([]peer.AddrInfo, []string) {
	var infos []peer.AddrInfo
	var bad []string
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		ai, err := peer.AddrInfoFromString(s)
		if err != nil {
			// AddrInfoFromString requires a /p2p/ component, so a bare
			// transport address is rejected here too.
			bad = append(bad, s)
			slog.Warn("invalid bootstrap peer, skipping", "addr", s, "error", err)
			continue
		}
		// Count accepted entries, not list rows: blank/duplicate lines
		// must not eat into the budget.
		if len(infos) >= maxBootstrapPeers {
			slog.Warn("too many bootstrap peers, ignoring the rest", "limit", maxBootstrapPeers)
			break
		}
		infos = append(infos, *ai)
	}
	return infos, bad
}
