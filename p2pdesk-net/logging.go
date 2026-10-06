package main

/*
#include <stdint.h>
typedef void (*p2pd_log_cb_t)(int32_t level, const uint8_t *msg, int32_t len);
static void call_log_cb(p2pd_log_cb_t cb, int32_t level, const uint8_t *msg, int32_t len) {
    cb(level, msg, len);
}
*/
import "C"

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/libp2p/go-libp2p/gologshim"
)

// Log level codes handed to the Rust bridge (independent of slog's values).
const (
	levelDebug = int32(0)
	levelInfo  = int32(1)
	levelWarn  = int32(2)
	levelError = int32(3)
)

// logCB is the optional Rust-side callback installed via
// p2pd_set_log_callback. Nil = records fall back to stderr (CLI tools).
var logCB atomic.Pointer[struct{}]

//export p2pd_set_log_callback
func p2pd_set_log_callback(cb unsafe.Pointer) {
	if cb == nil {
		logCB.Store(nil)
		return
	}
	logCB.Store((*struct{})(cb))
}

// bridgeHandler routes slog records to the Rust-side callback. Logs from
// libp2p-family packages (DHT gossip, identify churn, address chatter) are
// capped at Error unless the operator set P2PDESK_GO_LOG=debug — they repeat
// every few seconds and drown everything else. With no callback installed
// (CLI tools, tests), records fall back to plain stderr output.
type bridgeHandler struct {
	level    slog.Level
	attrs    []slog.Attr // pre-bound attrs (slog.With), rendered per record
	fallback slog.Handler
}

func newBridgeHandler(level slog.Level) *bridgeHandler {
	return &bridgeHandler{
		level:    level,
		fallback: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}),
	}
}

// noisyPrefixes lists packages whose logs are dominated by address/gossip
// noise rather than actionable messages.
var noisyPrefixes = []string{
	"github.com/libp2p/",
	"github.com/multiformats/",
	"github.com/quic-go/",
	"github.com/ipfs/",
}

func (h *bridgeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// Cheap pre-filter; Handle applies the stricter per-package routing.
	return level >= h.level
}

func (h *bridgeHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < minLevelFor(h.level, &r, h.attrs) {
		return nil
	}
	cb := logCB.Load()
	if cb == nil {
		return h.fallback.Handle(ctx, r)
	}

	// Render attributes into the message ("msg key=value key=value"), the
	// same shape slog's text handler produces.
	msg := r.Message
	if len(h.attrs) > 0 || r.NumAttrs() > 0 {
		var b strings.Builder
		b.WriteString(r.Message)
		appendAttr := func(a slog.Attr) {
			b.WriteByte(' ')
			b.WriteString(a.Key)
			b.WriteByte('=')
			b.WriteString(a.Value.String())
		}
		for _, a := range h.attrs {
			appendAttr(a)
		}
		r.Attrs(func(a slog.Attr) bool {
			appendAttr(a)
			return true
		})
		msg = b.String()
	}

	level := levelDebug
	switch {
	case r.Level >= slog.LevelError:
		level = levelError
	case r.Level >= slog.LevelWarn:
		level = levelWarn
	case r.Level >= slog.LevelInfo:
		level = levelInfo
	}

	raw := unsafe.Pointer(cb)
	fn := *(*C.p2pd_log_cb_t)(unsafe.Pointer(&raw))
	msgBytes := []byte(msg)
	var p *C.uint8_t
	if len(msgBytes) > 0 {
		p = (*C.uint8_t)(unsafe.Pointer(&msgBytes[0]))
	}
	// The callback runs synchronously (the Rust side consumes the buffer
	// before returning), so pointing into Go-owned memory is safe.
	C.call_log_cb(fn, C.int32_t(level), p, C.int32_t(len(msgBytes)))
	return nil
}

func (h *bridgeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &bridgeHandler{
		level:    h.level,
		attrs:    append(slices.Clone(h.attrs), attrs...),
		fallback: h.fallback.WithAttrs(attrs),
	}
}

func (h *bridgeHandler) WithGroup(name string) slog.Handler {
	return h
}

func hasNoisyPrefix(fn string) bool {
	for _, p := range noisyPrefixes {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	return false
}

// minLevelFor returns the minimum level accepted for a record: the global
// level, raised to Error for libp2p-family records in normal mode.
func minLevelFor(level slog.Level, r *slog.Record, attrs []slog.Attr) slog.Level {
	// Only the refresh manager's bounded, low-frequency lifecycle records.
	// Keep DHT per-request and peer/address chatter suppressed.
	refresh := false
	for _, a := range attrs {
		refresh = refresh || (a.Key == "logger" && a.Value.String() == "dht/RtRefreshManager")
	}
	r.Attrs(func(a slog.Attr) bool {
		refresh = refresh || (a.Key == "logger" && a.Value.String() == "dht/RtRefreshManager")
		return !refresh
	})
	if refresh {
		return level
	}
	min := level
	if level > slog.LevelDebug && isLibp2pFamily(r, attrs) {
		min = slog.LevelError
	}
	return min
}

// isLibp2pFamily reports whether a record originates from libp2p-family
// packages. Two signals: the call-site PC prefix, and the `logger=` attr
// gologshim attaches to every record it routes (the PC can point into the
// shim itself, so the attr is the reliable one).
func isLibp2pFamily(r *slog.Record, attrs []slog.Attr) bool {
	fs := runtime.CallersFrames([]uintptr{r.PC})
	if f, ok := fs.Next(); ok && f.Function != "" && hasNoisyPrefix(f.Function) {
		return true
	}
	for _, a := range attrs {
		if a.Key == "logger" {
			return true
		}
	}
	hasLogger := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			hasLogger = true
		}
		return !hasLogger
	})
	return hasLogger
}

func init() {
	level := slog.LevelInfo
	if os.Getenv("P2PDESK_GO_LOG") == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(newBridgeHandler(level)))
	// libp2p's own loggers (gologshim) bypass slog.Default() unless wired in
	// explicitly; without this their Warn/Error records would stay on stderr,
	// invisible in GUI builds. Records keep their libp2p call-site PC, so the
	// noisy-prefix routing in Handle still applies.
	gologshim.SetDefaultHandler(slog.Default().Handler())
}
