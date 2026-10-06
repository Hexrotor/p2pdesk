// p2pdesk-net: go-libp2p network module for p2pdesk, built as a C shared
// library (p2pdesk_net.dll / libp2pdesk_net.so) and loaded at runtime by
// the Rust side (src/p2pffi.rs).
//
// ABI v1: node lifecycle + connect/accept + stream read/write/close +
// identity signing + status. All strings cross the boundary via caller
// buffers (ptr + cap + written). Every exported function guards itself with
// a recover() so a Go panic never crosses the C ABI.

package main

/*
#include <stdint.h>

typedef struct {
    int32_t code;
    uint32_t detail_len;
} p2pd_result_t;

typedef struct {
    uint32_t connected_peers;
    uint32_t relay_connections;
    uint32_t direct_connections;
    uint8_t dht_ready;
    uint8_t relay_ready;
    // ABI v1 append-only: fields added after the initial release. The Rust
    // side zero-initializes the struct before the call, so an older module
    // writing only the first five fields leaves safe zero defaults here.
    // The reverse direction (newer module, older caller) is NOT safe — a
    // full 24-byte write would overrun an old caller's 16-byte struct — so
    // exe and module must always come from the same build (see docs/BUILD.md).
    uint32_t routing_table_size;
    uint8_t relay_reserved;
} p2pd_status_t;
*/
import "C"

import (
	"strings"
	"unsafe"

	"p2pdesk-net/nodekit"
)

// abiVersion is the ABI revision the Rust side checks (>= 1).
const abiVersion = 1

//export p2pd_abi_version
func p2pd_abi_version() C.uint32_t {
	return C.uint32_t(abiVersion)
}

//export p2pd_node_start
func p2pd_node_start(configJSON *C.uint8_t, configLen C.uint32_t, outNode *C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	cfg, err := nodekit.ParseConfig(cStr(configJSON, configLen))
	if err != nil {
		setLastError("%v", err)
		return result(errInternal)
	}
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if node != nil {
		setLastError("node already started")
		return result(errInternal)
	}
	n, err := startNode(cfg)
	if err != nil {
		setLastError("%v", err)
		return result(errInternal)
	}
	node = n
	*outNode = C.uint64_t(1)
	return result(errOK)
}

//export p2pd_node_stop
func p2pd_node_stop(nodeHandle C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	nodeMu.Lock()
	defer nodeMu.Unlock()
	if node == nil {
		return result(errOK) // idempotent
	}
	n := node
	node = nil
	n.stop()
	return result(errOK)
}

//export p2pd_node_status
func p2pd_node_status(nodeHandle C.uint64_t, out *C.p2pd_status_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	st := n.statusSnapshot()
	*out = C.p2pd_status_t{
		connected_peers:    C.uint32_t(st.ConnectedPeers),
		relay_connections:  C.uint32_t(st.RelayConnections),
		direct_connections: C.uint32_t(st.DirectConnections),
		dht_ready:          C.uint8_t(st.DHTReady),
		relay_ready:        C.uint8_t(st.RelayReady),
		routing_table_size: C.uint32_t(st.RoutingTableSize),
		relay_reserved:     C.uint8_t(st.RelayReserved),
	}
	return result(errOK)
}

//export p2pd_node_peer_id
func p2pd_node_peer_id(nodeHandle C.uint64_t, out *C.uint8_t, cap C.uint32_t, written *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	s := n.host.ID().String()
	buf := byteSlice(out, cap)
	w := copyLen(buf, s)
	*written = C.uint32_t(w)
	return result(errOK)
}

//export p2pd_connect
func p2pd_connect(nodeHandle C.uint64_t, peerID *C.uint8_t, peerLen C.uint32_t, timeoutMs C.uint32_t, outStream *C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	se, code := n.connect(cStr(peerID, peerLen), uint32(timeoutMs))
	if code != errOK {
		return result(code)
	}
	*outStream = C.uint64_t(se.id)
	return result(errOK)
}

//export p2pd_accept
func p2pd_accept(nodeHandle C.uint64_t, timeoutMs C.uint32_t, outStream *C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	se, code := n.accept(uint32(timeoutMs))
	if code != errOK {
		return result(code)
	}
	*outStream = C.uint64_t(se.id)
	return result(errOK)
}

//export p2pd_add_peer_address
func p2pd_add_peer_address(nodeHandle C.uint64_t, peerID *C.uint8_t, peerLen C.uint32_t, addr *C.uint8_t, addrLen C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	return result(n.addPeerAddress(cStr(peerID, peerLen), cStr(addr, addrLen)))
}

//export p2pd_node_sign
func p2pd_node_sign(nodeHandle C.uint64_t, msg *C.uint8_t, msgLen C.uint32_t, out *C.uint8_t, outCap C.uint32_t, outLen *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	code, w := n.sign(cBytes(msg, msgLen), byteSlice(out, outCap))
	if code != errOK {
		return result(code)
	}
	*outLen = C.uint32_t(w)
	return result(errOK)
}

//export p2pd_connected_peers
func p2pd_connected_peers(nodeHandle C.uint64_t, out *C.uint8_t, cap C.uint32_t, written *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	ids := make([]string, 0, 16)
	seen := make(map[string]struct{})
	for _, c := range n.host.Network().Conns() {
		id := c.RemotePeer().String()
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	buf := byteSlice(out, cap)
	w := copyLen(buf, strings.Join(ids, ","))
	*written = C.uint32_t(w)
	return result(errOK)
}

//export p2pd_last_error
func p2pd_last_error(out *C.uint8_t, cap C.uint32_t, written *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	msg := lastErrorSnapshot()
	buf := byteSlice(out, cap)
	w := copyLen(buf, msg)
	*written = C.uint32_t(w)
	return result(errOK)
}

// ===================== stream-level ABI =====================

//export p2pd_stream_peer_id
func p2pd_stream_peer_id(handle C.uint64_t, out *C.uint8_t, cap C.uint32_t, written *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	code, w := n.streamPeerID(uint64(handle), byteSlice(out, cap))
	if code != errOK {
		return result(code)
	}
	*written = C.uint32_t(w)
	return result(errOK)
}

//export p2pd_stream_remote_addr
func p2pd_stream_remote_addr(handle C.uint64_t, out *C.uint8_t, cap C.uint32_t, written *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	code, w := n.streamRemoteAddr(uint64(handle), byteSlice(out, cap))
	if code != errOK {
		return result(code)
	}
	*written = C.uint32_t(w)
	return result(errOK)
}

//export p2pd_stream_path_kind
func p2pd_stream_path_kind(handle C.uint64_t, kind *C.uint32_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	code, k := n.streamPathKind(uint64(handle))
	if code != errOK {
		return result(code)
	}
	*kind = C.uint32_t(k)
	return result(errOK)
}

//export p2pd_stream_read
func p2pd_stream_read(handle C.uint64_t, buf *C.uint8_t, len C.uint32_t, timeoutMs C.uint32_t) (r C.int64_t) {
	defer recoverInt64(&r)
	n := getNode()
	if n == nil {
		return C.int64_t(errClosed)
	}
	return C.int64_t(n.streamRead(uint64(handle), byteSlice(buf, len), uint32(timeoutMs)))
}

//export p2pd_stream_write
func p2pd_stream_write(handle C.uint64_t, buf *C.uint8_t, len C.uint32_t, timeoutMs C.uint32_t) (r C.int64_t) {
	defer recoverInt64(&r)
	n := getNode()
	if n == nil {
		return C.int64_t(errClosed)
	}
	return C.int64_t(n.streamWrite(uint64(handle), byteSlice(buf, len), uint32(timeoutMs)))
}

//export p2pd_stream_close_write
func p2pd_stream_close_write(handle C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errClosed)
	}
	se := n.getStream(uint64(handle))
	if se == nil {
		return result(errClosed)
	}
	if err := se.closeWrite(); err != nil {
		setLastError("close_write: %v", err)
		return result(errInternal)
	}
	return result(errOK)
}

//export p2pd_stream_close
func p2pd_stream_close(handle C.uint64_t) (r C.p2pd_result_t) {
	defer recoverResult(&r)
	n := getNode()
	if n == nil {
		return result(errOK) // idempotent
	}
	n.closeStream(uint64(handle))
	return result(errOK)
}

func main() {}

// ===================== C interop helpers =====================

func getNode() *Node {
	nodeMu.Lock()
	defer nodeMu.Unlock()
	return node
}

func result(code int) C.p2pd_result_t {
	return C.p2pd_result_t{code: C.int32_t(code), detail_len: 0}
}

// The exported functions use named return values and
// `defer recoverResult(&r)` (or recoverInt64 for read/write) so a panic
// yields errInternal instead of the zero value — which would otherwise be
// indistinguishable from success/EOF and silently fake a successful call.
func recoverResult(r *C.p2pd_result_t) {
	if rec := recover(); rec != nil {
		setLastError("panic: %v", rec)
		*r = C.p2pd_result_t{code: C.int32_t(errInternal), detail_len: 0}
	}
}

func recoverInt64(r *C.int64_t) {
	if rec := recover(); rec != nil {
		setLastError("panic: %v", rec)
		*r = C.int64_t(errInternal)
	}
}

// cStr views a caller buffer as a Go string.
func cStr(p *C.uint8_t, n C.uint32_t) string {
	if p == nil || n == 0 {
		return ""
	}
	return unsafe.String((*byte)(unsafe.Pointer(p)), uintptr(n))
}

// cBytes views a caller buffer as a Go byte slice.
func cBytes(p *C.uint8_t, n C.uint32_t) []byte {
	if p == nil || n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), uintptr(n))
}

// byteSlice views a caller output buffer as a Go byte slice.
func byteSlice(p *C.uint8_t, cap C.uint32_t) []byte {
	if p == nil || cap == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), uintptr(cap))
}
