package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestLogRouting(t *testing.T) {
	refresh := slog.NewRecord(time.Now(), slog.LevelInfo, "routing table liveness complete", 0)
	refresh.Add("logger", "dht/RtRefreshManager")
	if got := minLevelFor(slog.LevelInfo, &refresh, nil); got != slog.LevelInfo {
		t.Fatalf("refresh lifecycle record suppressed: %v", got)
	}
	libp2pChatter := slog.NewRecord(time.Now(), slog.LevelInfo, "identify failed", 0)
	libp2pChatter.Add("logger", "net/identify")

	// Normal mode: libp2p-family Info is capped at Error.
	if got := minLevelFor(slog.LevelInfo, &libp2pChatter, nil); got != slog.LevelError {
		t.Fatalf("libp2p chatter at Info: min level = %v, want Error", got)
	}
	// Normal mode: libp2p-family Error still passes.
	libp2pErr := slog.NewRecord(time.Now(), slog.LevelError, "fatal", 0)
	libp2pErr.Add("logger", "swarm2")
	if got := minLevelFor(slog.LevelInfo, &libp2pErr, nil); got != slog.LevelError {
		t.Fatalf("libp2p error: min level = %v, want Error", got)
	}
	// Debug mode unlocks everything.
	if got := minLevelFor(slog.LevelDebug, &libp2pChatter, nil); got != slog.LevelDebug {
		t.Fatalf("debug mode libp2p chatter: min level = %v, want Debug", got)
	}
	// Our own logs keep their levels.
	own := slog.NewRecord(time.Now(), slog.LevelInfo, "control stream open", 0)
	if got := minLevelFor(slog.LevelInfo, &own, nil); got != slog.LevelInfo {
		t.Fatalf("own Info: min level = %v, want Info", got)
	}
	// Pre-bound logger attr (gologshim WithAttrs path) also counts.
	prebound := slog.NewRecord(time.Now(), slog.LevelInfo, "relay churn", 0)
	preboundAttrs := []slog.Attr{slog.String("logger", "relay")}
	if got := minLevelFor(slog.LevelInfo, &prebound, preboundAttrs); got != slog.LevelError {
		t.Fatalf("prebound logger attr: min level = %v, want Error", got)
	}
}
