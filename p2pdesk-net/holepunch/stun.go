package holepunch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// STUN wire constants. Requests use the RFC 5389 magic cookie in the
// transaction id field; responses are parsed for both RFC 3489
// MAPPED-ADDRESS and RFC 5389 XOR-MAPPED-ADDRESS so either server
// generation works.
const (
	stunMsgBindingRequest  = 0x0001
	stunMsgBindingResponse = 0x0101
	stunAttrMappedAddress  = 0x0001
	stunAttrChangeRequest  = 0x0003
	stunAttrChangedAddress = 0x0005
	stunAttrXorMappedAddr  = 0x0020
	stunMagicCookie        = 0x2112A442
	stunChangeIPFlag       = 0x4
	stunChangePortFlag     = 0x2
	stunTIDLen             = 16 // magic cookie + 12-byte transaction id
)

// errOpenNAT is an internal sentinel: the probe observed its own address,
// meaning no NAT translation exists.
var errOpenNAT = errors.New("open nat")

// changeMode selects which RFC 3489 CHANGE-REQUEST probe to send.
type changeMode uint8

const (
	changeNone changeMode = iota
	changePort
	changeIPAndPort
)

// stunResult is one completed probe.
type stunResult struct {
	local           *net.UDPAddr // local socket address
	mapped          *net.UDPAddr // mapped address from the response
	changed         *net.UDPAddr // CHANGED-ADDRESS advertised by the server
	changeSucceeded bool         // response came from the changed address
}

// buildStunRequest assembles a binding request and returns the full 16-byte
// transaction identifier (magic cookie + tid) for response matching.
// change != changeNone adds a CHANGE-REQUEST attribute (RFC 3489: bit 2 =
// change IP, bit 1 = change port).
func buildStunRequest(change changeMode) (msg []byte, tid [stunTIDLen]byte, err error) {
	var attrs []byte
	if change != changeNone {
		var val uint32
		if change == changeIPAndPort {
			val = stunChangeIPFlag | stunChangePortFlag
		} else {
			val = stunChangePortFlag
		}
		attr := make([]byte, 8)
		binary.BigEndian.PutUint16(attr[0:2], stunAttrChangeRequest)
		binary.BigEndian.PutUint16(attr[2:4], 4)
		binary.BigEndian.PutUint32(attr[4:8], val)
		attrs = append(attrs, attr...)
	}
	msg = make([]byte, 20+len(attrs))
	binary.BigEndian.PutUint16(msg[0:2], stunMsgBindingRequest)
	binary.BigEndian.PutUint16(msg[2:4], uint16(len(attrs)))
	binary.BigEndian.PutUint32(msg[4:8], stunMagicCookie)
	if _, err = rand.Read(msg[8:20]); err != nil {
		return nil, tid, err
	}
	copy(tid[:], msg[4:20])
	copy(msg[20:], attrs)
	return msg, tid, nil
}

// parseStunResponse validates the header and extracts the mapped and
// changed addresses. ok=false means "not a binding response".
func parseStunResponse(data []byte) (*net.UDPAddr, *net.UDPAddr, bool) {
	if len(data) < 20 {
		return nil, nil, false
	}
	if binary.BigEndian.Uint16(data[0:2]) != stunMsgBindingResponse {
		return nil, nil, false
	}
	attrLen := int(binary.BigEndian.Uint16(data[2:4]))
	var mapped, changed *net.UDPAddr
	for off := 20; off+4 <= 20+attrLen && off+4 <= len(data); {
		atype := binary.BigEndian.Uint16(data[off : off+2])
		alen := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		off += 4
		if off+alen > len(data) {
			break
		}
		val := data[off : off+alen]
		switch atype {
		case stunAttrXorMappedAddr:
			if a, ok := parseMappedAddr(val, true); ok && mapped == nil {
				mapped = a
			}
		case stunAttrMappedAddress:
			if a, ok := parseMappedAddr(val, false); ok && mapped == nil {
				mapped = a
			}
		case stunAttrChangedAddress:
			if a, ok := parseMappedAddr(val, false); ok {
				changed = a
			}
		}
		off += (alen + 3) &^ 3 // attribute padding to 4-byte boundary
	}
	if mapped == nil {
		return nil, nil, false
	}
	return mapped, changed, true
}

// parseMappedAddr decodes an RFC 3489 MAPPED-ADDRESS-style value
// (1 reserved + 1 family + 2 port + 4 ip, possibly xor-masked).
func parseMappedAddr(val []byte, xor bool) (*net.UDPAddr, bool) {
	if len(val) != 8 {
		return nil, false
	}
	port := binary.BigEndian.Uint16(val[2:4])
	ip := net.IP(append([]byte(nil), val[4:8]...))
	if xor {
		port ^= 0x2112
		for i := range 4 {
			ip[i] ^= []byte{0x21, 0x12, 0xA4, 0x42}[i]
		}
	}
	return &net.UDPAddr{IP: ip, Port: int(port)}, true
}

// stunQueryOn runs one probe from the caller's socket. Only responses whose
// full transaction identifier matches the request are accepted: the packet
// source is intentionally not checked (change probes answer from the
// alternate address), so the TID is the sole authenticity filter against
// injected datagrams. The socket must not have an outstanding read deadline
// longer than the probe budget.
func stunQueryOn(ctx context.Context, server *net.UDPAddr, conn *net.UDPConn, change changeMode) (*stunResult, error) {
	req, tid, err := buildStunRequest(change)
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(req, server); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(stunTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	// The socket may be handed to another reader after this probe (the
	// punch receive loop, QUIC); a leftover deadline would kill that reader
	// with a spurious timeout. Clear it on every exit path.
	defer conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(buf[4:20], tid[:]) {
			continue // response for a different transaction
		}
		mapped, changed, ok := parseStunResponse(buf[:n])
		if !ok {
			continue // malformed or non-response datagram
		}
		r := &stunResult{
			local:   conn.LocalAddr().(*net.UDPAddr),
			mapped:  mapped,
			changed: changed,
		}
		if change != changeNone {
			// A change probe answers from the alternate address; a response
			// from the primary address means the change was not applied.
			r.changeSucceeded = !from.IP.Equal(server.IP) || from.Port != server.Port
		}
		return r, nil
	}
}

// probeServer runs one plain probe against a server from the given socket,
// with stunRequestRepeat attempts in total (packet loss tolerance). A
// cancelled context is not retried: the caller is going away.
func probeServer(ctx context.Context, server *net.UDPAddr, conn *net.UDPConn) (*stunResult, error) {
	var r *stunResult
	var err error
	for range stunRequestRepeat {
		r, err = stunQueryOn(ctx, server, conn, changeNone)
		if err == nil {
			return r, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}

// isOpenMapping reports a mapping that passes the source address through
// unchanged: same port as the local socket and an IP that belongs to this
// host. With a wildcard-bound socket the mapped IP never equals the local
// 0.0.0.0, so the check compares the port and tests the IP against the
// host's interfaces.
func isOpenMapping(r *stunResult) bool {
	return r.mapped.Port == r.local.Port && isLocalIP(r.mapped.IP)
}

// isLocalIP reports whether ip belongs to one of this host's interfaces.
func isLocalIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var aip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				aip = v.IP
			case *net.IPAddr:
				aip = v.IP
			}
			if aip != nil && aip.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// classifyCone grades the cone family using change probes from the same
// socket. Returns (type, subtype).
func classifyCone(ctx context.Context, server *net.UDPAddr, conn *net.UDPConn, plain *stunResult) (NATType, string) {
	if plain.changed == nil {
		// Server does not advertise a changed address: cannot grade further.
		return NATCone, ConePortRestricted
	}
	probe := func(mode changeMode) (*stunResult, error) {
		r, err := stunQueryOn(ctx, server, conn, mode)
		if err != nil {
			return nil, err
		}
		if !r.changeSucceeded {
			return nil, fmt.Errorf("change probe not applied")
		}
		return r, nil
	}
	if r, err := probe(changeIPAndPort); err == nil && r.mapped != nil &&
		r.mapped.IP.Equal(plain.mapped.IP) {
		if isOpenMapping(r) {
			return NATOpen, ""
		}
		if r.mapped.Port == r.local.Port {
			return NATCone, ConeNoPat
		}
		return NATCone, ConeFullCone
	}
	if _, err := probe(changePort); err == nil {
		return NATCone, ConeRestricted
	}
	return NATCone, ConePortRestricted
}

// DetectNAT classifies the local NAT. One socket probes every server, and
// a classification requires at least two responding servers; a single
// server's answer is not a consensus. Cone family: identical mappings
// across servers, graded by change probes. Endpoint-dependent mappings:
// hard-symmetric when the public IPs disagree across servers or the port
// spread stays within the frozen threshold but an extra socket binding
// (new ephemeral port) moves the mapping by (0,100) in a consistent
// direction, which grades easy-symmetric.
func DetectNAT(ctx context.Context, servers []string) (NATInfo, error) {
	var lastErr error
	addrs := make([]*net.UDPAddr, 0, len(servers))
	for _, s := range servers {
		addr, err := net.ResolveUDPAddr("udp4", s)
		if err != nil {
			lastErr = err
			continue
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no STUN servers configured")
		}
		return NATInfo{Type: NATUnknown}, lastErr
	}

	// One socket for the whole run: endpoint-dependence is only observable
	// when the same source socket talks to different servers.
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return NATInfo{Type: NATUnknown}, err
	}
	defer conn.Close()

	type probeResult struct {
		server *net.UDPAddr
		r      *stunResult
		err    error
	}
	results := make([]probeResult, 0, len(addrs))
	for _, addr := range addrs {
		r, err := probeServer(ctx, addr, conn)
		if err != nil {
			lastErr = err
			continue
		}
		if isOpenMapping(r) {
			return NATInfo{Type: NATOpen, PublicIP: r.mapped.IP.String(), PublicPort: r.mapped.Port}, nil
		}
		results = append(results, probeResult{server: addr, r: r})
	}

	if len(results) < 2 {
		// Distinct responding servers < 2 → Unknown.
		if lastErr == nil {
			lastErr = fmt.Errorf("only %d of %d STUN servers responded", len(results), len(addrs))
		}
		return NATInfo{Type: NATUnknown}, lastErr
	}

	info := NATInfo{PublicIP: results[0].r.mapped.IP.String(), PublicPort: results[0].r.mapped.Port}

	// Cone family: every server reports the same mapping.
	minPort, maxPort := results[0].r.mapped.Port, results[0].r.mapped.Port
	allSame := true
	for _, pr := range results {
		if !pr.r.mapped.IP.Equal(results[0].r.mapped.IP) {
			allSame = false
		}
		if pr.r.mapped.Port < minPort {
			minPort = pr.r.mapped.Port
		}
		if pr.r.mapped.Port > maxPort {
			maxPort = pr.r.mapped.Port
		}
	}
	// Different public IPs across servers: the mapping is endpoint-dependent
	// at the address level, beyond what any port prediction can follow.
	if !allSame {
		info.Type = NATSymmetricHard
		return info, nil
	}
	if minPort == maxPort {
		t, sub := classifyCone(ctx, results[0].server, conn, results[0].r)
		info.Type, info.Subtype = t, sub
		return info, nil
	}

	// Endpoint-dependent. The spread across servers on one socket is the
	// hard-symmetric discriminator (> 15).
	if maxPort-minPort > hardSymSpreadThreshold {
		info.Type = NATSymmetricHard
		return info, nil
	}

	// Extra-bind test: a second socket (fresh ephemeral port) against one
	// responding server reveals the allocation direction. The cone family
	// is already excluded at this point, so a failed probe still grades
	// symmetric; without a direction the conservative answer is hard.
	extraSock, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return NATInfo{Type: NATSymmetricHard}, fmt.Errorf("extra-bind socket: %w", err)
	}
	defer extraSock.Close()
	extra, err := probeServer(ctx, results[0].server, extraSock)
	if err != nil {
		return NATInfo{Type: NATSymmetricHard}, fmt.Errorf("extra-bind probe failed: %w", err)
	}
	switch {
	case extra.mapped.Port-maxPort > 0 && extra.mapped.Port-maxPort < easySymDiffMax:
		info.Type = NATSymmetricEasyInc
	case minPort-extra.mapped.Port > 0 && minPort-extra.mapped.Port < easySymDiffMax:
		info.Type = NATSymmetricEasyDec
	default:
		info.Type = NATSymmetricHard
	}
	return info, nil
}

// MapSocket reports the public mapping of an existing socket. Used to learn
// a listener socket's mapped port before the socket is handed to QUIC.
func MapSocket(ctx context.Context, server string, conn *net.UDPConn) (int, error) {
	addr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return 0, err
	}
	r, err := stunQueryOn(ctx, addr, conn, changeNone)
	if err != nil {
		return 0, err
	}
	return r.mapped.Port, nil
}

// GetUDPPortMapping reports the current public mapping of a fresh local
// socket, used by an easy-symmetric peer to learn its live base port right
// before a punch round.
func GetUDPPortMapping(ctx context.Context, server string) (int, error) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	return MapSocket(ctx, server, c)
}
