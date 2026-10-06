package holepunch

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// quicLoopback exercises the full post-punch transport on loopback: a QUIC
// listener over one socket and a dial over another, token-authenticated via
// the first stream. wantOK selects the positive and negative cases.
func quicLoopback(t *testing.T, dialToken, acceptToken []byte, wantOK bool) {
	t.Helper()
	lnSock := loopbackUDP(t)
	defer lnSock.Close()
	dSock := loopbackUDP(t)
	defer dSock.Close()

	ln, err := DirectListen(lnSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		conn, st, err := AcceptAuthQUIC(ctx, ln, acceptToken)
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.CloseWithError(0, "done")
		// Echo one message back to prove the stream carries data, then
		// block until the client closes the connection.
		buf := make([]byte, 16)
		n, err := io.ReadFull(st, buf[:4])
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := st.Write(buf[:n]); err != nil {
			serverDone <- err
			return
		}
		_, _ = st.Read(buf[:1]) // unblocks when the client closes
		serverDone <- nil
	}()

	conn, st, err := DirectDialQUIC(ctx, dSock, lnSock.LocalAddr(), dialToken)
	if wantOK {
		if err != nil {
			t.Fatalf("DirectDialQUIC: %v", err)
		}
		// Roundtrip data through the authenticated first stream.
		if _, err := st.Write([]byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		echo := make([]byte, 4)
		if _, err := io.ReadFull(st, echo); err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if string(echo) != "ping" {
			t.Fatalf("echo %q", echo)
		}
		// Close so the server's blocking read returns, then collect it.
		_ = conn.CloseWithError(0, "done")
		if err := <-serverDone; err != nil {
			t.Fatalf("server side: %v", err)
		}
	} else {
		if err == nil {
			_ = conn.CloseWithError(0, "done")
			t.Fatal("dial succeeded despite token mismatch")
		}
		select {
		case err := <-serverDone:
			if err == nil {
				t.Fatal("server accepted a mismatched token")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("server never rejected the mismatched token")
		}
	}
}

func TestQuicLoopbackTokenOK(t *testing.T) {
	tok, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	quicLoopback(t, tok, tok, true)
}

func TestQuicLoopbackTokenMismatch(t *testing.T) {
	tok, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	other := make([]byte, len(tok))
	copy(other, tok)
	other[0] ^= 0xFF
	quicLoopback(t, other, tok, false)
}

func TestGenerateSessionToken(t *testing.T) {
	t1, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(t1) != sessionTokenLen {
		t.Fatalf("token length %d, want %d", len(t1), sessionTokenLen)
	}
	t2, err := GenerateSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(t1, t2) {
		t.Fatal("two tokens identical")
	}
}
