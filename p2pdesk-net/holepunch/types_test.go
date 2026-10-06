package holepunch

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestPunchPacketRoundtrip(t *testing.T) {
	body, err := randomBody()
	if err != nil {
		t.Fatal(err)
	}
	pkt := MakePunchPacket(0xDEADBEEF, body)
	if len(pkt) != punchPacketSize {
		t.Fatalf("packet size %d, want %d", len(pkt), punchPacketSize)
	}
	tid, ok := ParsePunchPacket(pkt[:])
	if !ok || tid != 0xDEADBEEF {
		t.Fatalf("roundtrip failed: ok=%v tid=%#x", ok, tid)
	}
}

func TestParsePunchPacketRejectsNoise(t *testing.T) {
	// Wrong length.
	if _, ok := ParsePunchPacket(make([]byte, punchPacketSize-1)); ok {
		t.Fatal("short packet accepted")
	}
	// Wrong msg type.
	pkt := MakePunchPacket(1, [punchBodyLen]byte{})
	pkt[4] = 9
	if _, ok := ParsePunchPacket(pkt[:]); ok {
		t.Fatal("wrong msg type accepted")
	}
	// Wrong body length field.
	pkt = MakePunchPacket(1, [punchBodyLen]byte{})
	pkt[6] = 0
	if _, ok := ParsePunchPacket(pkt[:]); ok {
		t.Fatal("wrong body length accepted")
	}
	// Random noise of the right length.
	noise := make([]byte, punchPacketSize)
	for i := range noise {
		noise[i] = byte(i * 7)
	}
	if _, ok := ParsePunchPacket(noise); ok {
		t.Fatal("noise accepted")
	}
}

func TestDeterminePunchMethod(t *testing.T) {
	cases := []struct {
		my, peer NATType
		want     PunchMethod
	}{
		{NATCone, NATCone, PunchConeToCone},
		{NATUnknown, NATCone, PunchConeToCone}, // unknown graded as cone
		{NATUnknown, NATUnknown, PunchConeToCone},
		{NATSymmetricHard, NATCone, PunchSymToCone},
		{NATCone, NATSymmetricHard, PunchSymToCone},
		{NATSymmetricEasyInc, NATCone, PunchSymToCone},
		{NATUnknown, NATSymmetricHard, PunchSymToCone},
		{NATSymmetricEasyInc, NATSymmetricEasyInc, PunchEasySymToEasySym},
		{NATSymmetricEasyDec, NATSymmetricEasyDec, PunchEasySymToEasySym},
		{NATSymmetricHard, NATSymmetricEasyInc, PunchNone}, // both sym, one hard
		{NATSymmetricHard, NATSymmetricHard, PunchNone},
		{NATOpen, NATCone, PunchNone},
		{NATCone, NATOpen, PunchNone},
		{NATOpen, NATOpen, PunchNone},
	}
	for _, c := range cases {
		if got := DeterminePunchMethod(c.my, c.peer); got != c.want {
			t.Errorf("DeterminePunchMethod(%v, %v) = %v, want %v", c.my, c.peer, got, c.want)
		}
	}
}

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{
		time.Second, time.Second, 2 * time.Second, 4 * time.Second,
		4 * time.Second, 8 * time.Second, 8 * time.Second, 16 * time.Second,
		64 * time.Second, 64 * time.Second, 64 * time.Second,
	}
	for i, w := range want {
		if got := backoffDelay(i); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestShufflePorts(t *testing.T) {
	ports := ShufflePorts()
	if len(ports) != 65535 {
		t.Fatalf("vector length %d, want 65535", len(ports))
	}
	seen := make(map[int]bool, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			t.Fatalf("port %d out of range", p)
		}
		if seen[p] {
			t.Fatalf("port %d duplicated", p)
		}
		seen[p] = true
	}
}

func TestRandomBody(t *testing.T) {
	b1, err := randomBody()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := randomBody()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(b1[:], b2[:]) {
		t.Fatal("two random bodies identical")
	}
}

func TestSignalMsgJSONRoundtrip(t *testing.T) {
	msg := SignalMsg{
		Kind:          SigHello,
		MyNAT:         NATInfo{Type: NATCone, Subtype: ConeFullCone, PublicIP: "1.2.3.4", PublicPort: 12345},
		Method:        PunchSymToCone,
		TID:           42,
		Token:         []byte("0123456789abcdef"),
		BasePort:      40000,
		Round:         1,
		PortIndex:     100,
		MaxK2:         700,
		NextPortIndex: 800,
	}
	data, err := json.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	var got SignalMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != msg.Kind || got.MyNAT.PublicPort != 12345 || got.TID != 42 ||
		got.BasePort != 40000 || got.NextPortIndex != 800 || got.Method != PunchSymToCone {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if string(got.Token) != "0123456789abcdef" {
		t.Fatalf("token mangled: %q", got.Token)
	}
}
