package holepunch

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeStunServer is an in-process STUN server with a scriptable mapping
// policy and optional RFC 3489 CHANGE support. The mapping is a function of
// the request's source port and the server's own id, so two servers can
// model endpoint-dependent (symmetric) behavior for the same client socket.
// respondPort/respondIP simulate the NAT's inbound filter: a restricted NAT
// answers change-port probes but silently drops change-ip probes.
type fakeStunServer struct {
	mu          sync.Mutex
	primary     *net.UDPConn
	changed     *net.UDPConn
	addr        *net.UDPAddr
	altAddr     *net.UDPAddr // nil unless CHANGE is supported
	id          int          // server index for the mapping function
	mappedIP    net.IP
	fixed       int  // mapping port when step == 0
	step        int  // 0 = fixed, 1 = easy-inc, -1 = easy-dec, 2 = hard
	advert      bool // advertise CHANGED-ADDRESS in responses
	respondPort bool // answer change-port probes from the alternate address
	respondIP   bool // answer change-ip probes from the alternate address
	echoSource  bool // map the request source back (open internet simulation)
	done        chan struct{}
}

func newFakeStun(t *testing.T, id int, mappedIP string, fixed, step int, advert, respondPort, respondIP bool) *fakeStunServer {
	t.Helper()
	s := &fakeStunServer{
		id:          id,
		mappedIP:    net.ParseIP(mappedIP),
		fixed:       fixed,
		step:        step,
		advert:      advert,
		respondPort: respondPort,
		respondIP:   respondIP,
		done:        make(chan struct{}),
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("primary listen: %v", err)
	}
	s.primary = c
	s.addr = c.LocalAddr().(*net.UDPAddr)
	if advert {
		alt, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("alternate listen: %v", err)
		}
		s.changed = alt
		s.altAddr = alt.LocalAddr().(*net.UDPAddr)
	}
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func (s *fakeStunServer) close() {
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	if s.changed != nil {
		_ = s.changed.Close()
	}
	_ = s.primary.Close()
	s.mu.Unlock()
}

// allocMapping returns the mapped port for a request coming from srcPort.
// The policies model the classification inputs:
//   - fixed: cone (same mapping regardless of source or server)
//   - easy-inc: endpoint-dependent, small cross-server spread (10), the
//     extra-bind diff equals the OS port delta δ, so the grade stays
//     easy-inc for any δ in [1,99] — no flakiness from ephemeral allocation
//   - easy-dec: same shape, shrinking allocation
//   - hard: chaotic hash (cross-server spread far beyond the threshold)
func (s *fakeStunServer) allocMapping(srcPort int) int {
	switch s.step {
	case 0:
		return s.fixed
	case 1:
		// srcPort ≥ 49152 → 40000+srcPort ∈ [89152,105535], always exactly
		// one mod-65535 wrap; the +10 id shift keeps the cross-server spread
		// at 10 while the extra-bind diff stays exactly δ.
		return (40000+srcPort-10*s.id)%65535 + 1
	case -1:
		// 40000-srcPort ∈ [-25535,-9152], always exactly one negative wrap;
		// same invariants as the inc case, shrinking with srcPort.
		return ((40000-srcPort+10*s.id)%65535+65535)%65535 + 1
	default:
		h := uint64(srcPort)*2654435761 + uint64(s.id)*104729
		return int((h>>16)&0xFFFF)%65535 + 1
	}
}

// parseStunRequest returns the change-request bits (0 = none) and the tid.
func parseStunRequest(buf []byte) (changeBits uint32, tid []byte) {
	attrLen := int(binary.BigEndian.Uint16(buf[2:4]))
	for off := 20; off+4 <= 20+attrLen && off+4 <= len(buf); {
		atype := binary.BigEndian.Uint16(buf[off : off+2])
		alen := int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		off += 4
		if off+alen > len(buf) {
			break
		}
		if atype == stunAttrChangeRequest && alen == 4 {
			changeBits = binary.BigEndian.Uint32(buf[off : off+4])
		}
		off += (alen + 3) &^ 3
	}
	return changeBits, buf[8:20]
}

// buildStunResponse crafts a binding response with XOR-MAPPED-ADDRESS.
func buildStunResponse(tid []byte, mapped *net.UDPAddr, changed *net.UDPAddr) []byte {
	attrs := make([]byte, 0, 16)
	xaddr := func(a *net.UDPAddr) []byte {
		val := make([]byte, 8)
		port := uint16(a.Port) ^ 0x2112
		binary.BigEndian.PutUint16(val[2:4], port)
		ip := a.IP.To4()
		for i := range 4 {
			val[4+i] = ip[i] ^ []byte{0x21, 0x12, 0xA4, 0x42}[i]
		}
		return val
	}
	attr := make([]byte, 12)
	binary.BigEndian.PutUint16(attr[0:2], stunAttrXorMappedAddr)
	binary.BigEndian.PutUint16(attr[2:4], 8)
	copy(attr[4:], xaddr(mapped))
	attrs = append(attrs, attr...)
	if changed != nil {
		attr = make([]byte, 12)
		binary.BigEndian.PutUint16(attr[0:2], stunAttrChangedAddress)
		binary.BigEndian.PutUint16(attr[2:4], 8)
		ip := changed.IP.To4()
		attr[5] = 0x01
		binary.BigEndian.PutUint16(attr[6:8], uint16(changed.Port))
		copy(attr[8:], ip)
		attrs = append(attrs, attr...)
	}
	resp := make([]byte, 20+len(attrs))
	binary.BigEndian.PutUint16(resp[0:2], stunMsgBindingResponse)
	binary.BigEndian.PutUint16(resp[2:4], uint16(len(attrs)))
	copy(resp[4:8], []byte{0x21, 0x12, 0xA4, 0x42})
	copy(resp[8:20], tid)
	copy(resp[20:], attrs)
	return resp
}

func (s *fakeStunServer) serve() {
	buf := make([]byte, 2048)
	for {
		n, from, err := s.primary.ReadFromUDP(buf)
		if err != nil {
			return
		}
		changeBits, tid := parseStunRequest(buf[:n])
		s.mu.Lock()
		respFrom := s.primary
		var changedAdvert *net.UDPAddr
		if s.advert {
			changedAdvert = s.altAddr
		}
		// A restricted NAT answers change-port but drops change-ip; this
		// is simulated by suppressing the response entirely.
		drop := false
		if changeBits != 0 && s.changed != nil {
			changeIP := changeBits&stunChangeIPFlag != 0
			changePort := changeBits&stunChangePortFlag != 0
			switch {
			case changeIP && !s.respondIP:
				drop = true
			case changePort && !s.respondPort:
				drop = true
			default:
				respFrom = s.changed
			}
		}
		echo := s.echoSource
		s.mu.Unlock()
		if drop {
			continue
		}
		mapped := &net.UDPAddr{IP: s.mappedIP, Port: s.allocMapping(from.Port)}
		if echo {
			mapped = from
		}
		resp := buildStunResponse(tid, mapped, changedAdvert)
		_, _ = respFrom.WriteToUDP(resp, from)
	}
}

// detectWithServer runs DetectNAT against the given servers.
func detectWithServer(t *testing.T, servers ...string) NATInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := DetectNAT(ctx, servers)
	if err != nil {
		t.Fatalf("DetectNAT: %v", err)
	}
	return info
}

// The mapping IP must differ from the loopback source so detection grades
// NAT behavior instead of reporting an open internet.
const fakePublicIP = "198.51.100.7"

// conePair returns two fake servers with an identical fixed mapping.
func conePair(t *testing.T, advert, respondPort, respondIP bool) (*fakeStunServer, *fakeStunServer) {
	t.Helper()
	return newFakeStun(t, 0, fakePublicIP, 40000, 0, advert, respondPort, respondIP),
		newFakeStun(t, 1, fakePublicIP, 40000, 0, advert, respondPort, respondIP)
}

// symPair returns two fake servers with the given per-source step policy.
func symPair(t *testing.T, step int) (*fakeStunServer, *fakeStunServer) {
	t.Helper()
	return newFakeStun(t, 0, fakePublicIP, 0, step, false, false, false),
		newFakeStun(t, 1, fakePublicIP, 0, step, false, false, false)
}

func TestDetectNATOpen(t *testing.T) {
	// The server maps the request source back: no NAT translation.
	s := newFakeStun(t, 0, "0.0.0.0", 0, 0, false, false, false)
	s.echoSource = true
	info := detectWithServer(t, s.addr.String())
	if info.Type != NATOpen {
		t.Fatalf("want NATOpen, got %v", info.Type)
	}
}

func TestDetectNATFullCone(t *testing.T) {
	s1, s2 := conePair(t, true, true, true)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATCone || info.Subtype != ConeFullCone {
		t.Fatalf("want cone/full-cone, got %v/%q", info.Type, info.Subtype)
	}
}

func TestDetectNATRestricted(t *testing.T) {
	// Advertises a changed address, answers change-port but drops change-ip.
	s1, s2 := conePair(t, true, true, false)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATCone || info.Subtype != ConeRestricted {
		t.Fatalf("want cone/restricted, got %v/%q", info.Type, info.Subtype)
	}
}

func TestDetectNATPortRestricted(t *testing.T) {
	// No CHANGED-ADDRESS advertisement at all: cannot grade further.
	s1, s2 := conePair(t, false, false, false)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATCone || info.Subtype != ConePortRestricted {
		t.Fatalf("want cone/port-restricted, got %v/%q", info.Type, info.Subtype)
	}
}

func TestDetectNATEasyInc(t *testing.T) {
	s1, s2 := symPair(t, 1)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATSymmetricEasyInc {
		t.Fatalf("want easy-inc, got %v", info.Type)
	}
}

func TestDetectNATEasyDec(t *testing.T) {
	s1, s2 := symPair(t, -1)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATSymmetricEasyDec {
		t.Fatalf("want easy-dec, got %v", info.Type)
	}
}

func TestDetectNATHardSym(t *testing.T) {
	s1, s2 := symPair(t, 2)
	info := detectWithServer(t, s1.addr.String(), s2.addr.String())
	if info.Type != NATSymmetricHard {
		t.Fatalf("want hard-sym, got %v", info.Type)
	}
}

func TestDetectNATSingleRespondingServer(t *testing.T) {
	// One live server plus one dead server: the consensus rule demands
	// two responding servers, so the result is Unknown.
	s1, _ := conePair(t, false, false, false)
	info, err := DetectNAT(t.Context(), []string{s1.addr.String(), "127.0.0.1:1"})
	if err == nil {
		t.Fatal("want error with only one responding server")
	}
	if info.Type != NATUnknown {
		t.Fatalf("want NATUnknown, got %v", info.Type)
	}
}

func TestDetectNATUnreachable(t *testing.T) {
	// Dead servers yield NATUnknown, not a bogus classification.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	info, err := DetectNAT(ctx, []string{"127.0.0.1:1", "127.0.0.1:2"})
	if err == nil {
		t.Fatal("want error for unreachable servers")
	}
	if info.Type != NATUnknown {
		t.Fatalf("want NATUnknown, got %v", info.Type)
	}
}

func TestStunResponseTIDMismatchIgnored(t *testing.T) {
	// A response whose transaction id does not match the request must be
	// ignored, not accepted (anti-spoof filter).
	s := newFakeStun(t, 0, fakePublicIP, 40000, 0, false, false, false)
	// Spoof one bogus response first, then the real server answers.
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	addr, err := net.ResolveUDPAddr("udp4", s.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	bogus := buildStunResponse(make([]byte, 12), &net.UDPAddr{IP: net.ParseIP(fakePublicIP), Port: 9999}, nil)
	if _, err := conn.WriteToUDP(bogus, addr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := stunQueryOn(ctx, addr, conn, changeNone)
	if err != nil {
		t.Fatalf("stunQueryOn: %v", err)
	}
	if r.mapped.Port != 40000 {
		t.Fatalf("accepted the bogus response: mapped port %d", r.mapped.Port)
	}
}

func TestParseStunResponseMalformed(t *testing.T) {
	if _, _, ok := parseStunResponse([]byte("garbage")); ok {
		t.Fatal("malformed data parsed as a response")
	}
	// Valid header, no mapped attribute.
	hdr := make([]byte, 20)
	binary.BigEndian.PutUint16(hdr[0:2], stunMsgBindingResponse)
	if _, _, ok := parseStunResponse(hdr); ok {
		t.Fatal("response without mapped address accepted")
	}
	// Truncated attribute stream.
	binary.BigEndian.PutUint16(hdr[2:4], 100)
	if _, _, ok := parseStunResponse(hdr); ok {
		t.Fatal("truncated attribute stream accepted")
	}
}

func TestMapSocket(t *testing.T) {
	s := newFakeStun(t, 0, fakePublicIP, 40000, 0, false, false, false)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	port, err := MapSocket(ctx, s.addr.String(), conn)
	if err != nil {
		t.Fatalf("MapSocket: %v", err)
	}
	if port != 40000 {
		t.Fatalf("want mapped port 40000, got %d", port)
	}
	// Regression: MapSocket must leave no read deadline behind. A leftover
	// stunTimeout deadline on a socket handed to the punch receive loop or
	// QUIC would kill that reader. SetReadDeadline replaces (does not clear)
	// earlier deadlines, so the observable check is a bare blocking read:
	// with a leftover deadline it errors out within stunTimeout; cleaned,
	// it blocks forever (observed via a window longer than stunTimeout).
	// The deferred conn.Close() at test end releases the reader goroutine.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadFromUDP(make([]byte, 1))
		readErr <- err
	}()
	select {
	case err := <-readErr:
		t.Fatalf("probe socket died with %v: MapSocket left a read deadline behind", err)
	case <-time.After(stunTimeout + time.Second):
	}
}
