package rtrefresh

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	kb "github.com/libp2p/go-libp2p-kbucket"
	"github.com/libp2p/go-libp2p/core/connmgr"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/test"
	pstore "github.com/libp2p/go-libp2p/p2p/host/peerstore"
	"github.com/stretchr/testify/require"
)

type checkHost struct {
	host.Host
	connect func(context.Context, peer.AddrInfo) error
}

func (h checkHost) Connect(ctx context.Context, p peer.AddrInfo) error { return h.connect(ctx, p) }
func (h checkHost) ConnManager() connmgr.ConnManager                   { return &connmgr.NullConnMgr{} }

func staleTable(t *testing.T, count int) *kb.RoutingTable {
	t.Helper()
	rt, err := kb.NewRoutingTable(128, kb.ConvertPeerID(test.RandPeerIDFatal(t)), time.Hour, pstore.NewMetrics(), 100*time.Hour, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	for i := 0; i < count; i++ {
		p := test.RandPeerIDFatal(t)
		added, err := rt.TryAddPeer(p, true, false)
		require.NoError(t, err)
		require.True(t, added)
		rt.UpdateLastSuccessfulOutboundQueryAt(p, time.Now().Add(-24*time.Hour))
	}
	return rt
}

func TestLivenessQueueDoesNotConsumePeerTimeout(t *testing.T) {
	rt := staleTable(t, 15)
	var active, peak, completed atomic.Int32
	var shortDeadline atomic.Bool
	h := checkHost{connect: func(ctx context.Context, _ peer.AddrInfo) error {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 9*time.Second {
			shortDeadline.Store(true)
		}
		return nil
	}}
	r := &RtRefreshManager{rt: rt, h: h, peerCheckConcurrency: 3, successfulOutboundQueryGracePeriod: time.Hour,
		refreshPingFnc: func(ctx context.Context, _ peer.ID) error {
			defer active.Add(-1)
			// The fifth batch waits >10s. Queued peers still get a fresh
			// 10s deadline when admitted; a global timeout would evict them.
			select {
			case <-time.After(2700 * time.Millisecond):
				completed.Add(1)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	r.pingAndEvictPeers(t.Context())
	require.EqualValues(t, 3, peak.Load())
	require.EqualValues(t, 15, completed.Load())
	require.False(t, shortDeadline.Load())
	require.Equal(t, 15, rt.Size())
}

func TestLivenessCancellationKeepsQueuedAndActivePeers(t *testing.T) {
	rt := staleTable(t, 12)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{}, 3)
	h := checkHost{connect: func(ctx context.Context, _ peer.AddrInfo) error {
		select {
		case started <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	r := &RtRefreshManager{rt: rt, h: h, peerCheckConcurrency: 3, successfulOutboundQueryGracePeriod: time.Hour}
	done := make(chan struct{})
	go func() { r.pingAndEvictPeers(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		<-started
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop queued checks")
	}
	require.Equal(t, 12, rt.Size())
}
