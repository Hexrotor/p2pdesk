package main

import (
	"fmt"
	"sync"
)

// Stable negative error codes. The Rust side (src/p2pffi.rs,
// libs/hbb_common/src/p2p.rs) mirrors these.
const (
	errOK          = 0
	errTimeout     = -1
	errClosed      = -2
	errNotFound    = -3
	errInternal    = -4
	errUnsupported = -5
)

var lastErr struct {
	sync.Mutex
	msg string
}

// setLastError records the most recent failure for p2pd_last_error.
func setLastError(format string, args ...any) {
	lastErr.Lock()
	lastErr.msg = fmt.Sprintf(format, args...)
	lastErr.Unlock()
}

// lastErrorSnapshot returns the recorded message.
func lastErrorSnapshot() string {
	lastErr.Lock()
	defer lastErr.Unlock()
	return lastErr.msg
}
