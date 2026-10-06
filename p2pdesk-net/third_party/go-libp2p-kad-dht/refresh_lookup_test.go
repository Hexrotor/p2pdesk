package dht

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	dhtnet "github.com/libp2p/go-libp2p-kad-dht/internal/net"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	kb "github.com/libp2p/go-libp2p-kbucket"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/stretchr/testify/require"
)

type refreshCountingSender struct {
	pb.MessageSenderWithDisconnect
	calls *atomic.Int32
}

const refreshTestKey = "p2pdesk-maintenance-convergence-test"

func (m refreshCountingSender) SendRequest(ctx context.Context, p peer.ID, req *pb.Message) (*pb.Message, error) {
	if string(req.GetKey()) == refreshTestKey {
		m.calls.Add(1)
	}
	return m.MessageSenderWithDisconnect.SendRequest(ctx, p, req)
}

func TestRefreshLookupConvergesWithoutPublicationFollowup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var calls atomic.Int32
	root := setupDHT(ctx, t, false, BucketSize(3), Concurrency(1), Resiliency(1), disableFixLowPeersRoutine(t),
		RoutingTableRefreshWithoutFollowup(), RoutingTableCheckConcurrency(2),
		WithCustomMessageSender(func(h host.Host, protos []protocol.ID) pb.MessageSenderWithDisconnect {
			return refreshCountingSender{dhtnet.NewMessageSenderImpl(h, protos), &calls}
		}))
	for i := 0; i < 3; i++ {
		remote := setupDHT(ctx, t, false, BucketSize(3), disableFixLowPeersRoutine(t))
		connect(t, ctx, root, remote)
	}
	// With alpha=beta=1, the nearest seed responds and convergence is met.
	// The other two seeds are needed only by the public API's followup.
	calls.Store(0)
	require.NoError(t, root.refreshLookup(ctx, refreshTestKey))
	maintenanceCalls := calls.Load()
	require.EqualValues(t, 1, maintenanceCalls)
	calls.Store(0)
	peers, err := root.GetClosestPeers(ctx, refreshTestKey)
	require.NoError(t, err)
	require.Len(t, peers, 3)
	require.EqualValues(t, 3, calls.Load(), "public GetClosestPeers must retain all top-K followups")
	// Two otherwise healthy peers stall. Once the closest responds, an
	// endpoint maintenance lookup must cancel these surplus requests rather
	// than wait for the entire refresh-query timeout on every bucket.
	root.alpha = 3
	closest := root.routingTable.NearestPeers(kb.ConvertKey(refreshTestKey), 1)[0]
	slowStarted := make(chan struct{}, 2)
	queryCtx, queryCancel := context.WithTimeout(ctx, 2*time.Second)
	defer queryCancel()
	started := time.Now()
	res, _, err := root.runQuery(queryCtx, refreshTestKey, func(c context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
		if p == closest {
			for i := 0; i < 2; i++ {
				select {
				case <-slowStarted:
				case <-c.Done():
					return nil, c.Err()
				}
			}
			return nil, nil
		}
		slowStarted <- struct{}{}
		<-c.Done()
		return nil, c.Err()
	}, func(*qpeerset.QueryPeerset) bool { return false }, true)
	require.NoError(t, err)
	require.NoError(t, queryCtx.Err())
	require.True(t, res.completed)
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, 3, root.routingTable.Size(), "cancelled surplus requests must not evict healthy peers")
	cancelled, stop := context.WithCancel(ctx)
	stop()
	require.Error(t, root.refreshLookup(cancelled, refreshTestKey))
}
