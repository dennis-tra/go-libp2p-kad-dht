package dht

import (
	"context"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"

	kb "github.com/libp2p/go-libp2p-kbucket"
)

// GetClosestPeers is a Kademlia 'node lookup' operation. Returns a channel of
// the K closest peers to the given key.
//
// If the context is canceled, this function will return the context error along
// with the closest K peers it has found so far.
func (dht *IpfsDHT) GetClosestPeers(ctx context.Context, key string) ([]peer.ID, error) {
	if key == "" {
		return nil, fmt.Errorf("can't lookup empty key")
	}
	//TODO: I can break the interface! return []peer.ID
	lookupRes, err := dht.runLookupWithFollowup(ctx, key,
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})
			if testLog {
				fmt.Printf("Getting closest peers for cid %v from %v\n", peer.ID(key).String(), p.String())
			}

			peers, err := dht.protoMessenger.GetClosestPeers(ctx, p, peer.ID(key))
			if err != nil {
				logger.Debugf("error getting closer peers: %s", err)
				return nil, err
			}

			if testLog {
				fmt.Printf("Got %v closest peers to cid %v: ", len(peers), peer.ID(key).String())
				for _, peer := range peers {
					fmt.Printf("%v ", peer.ID.String())
				}
				fmt.Println()
			}

			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, err
		},
		func() bool { return false },
	)

	if err != nil {
		return nil, err
	}

	if ctx.Err() == nil && lookupRes.completed {
		// refresh the cpl for this key as the query was successful
		dht.routingTable.ResetCplRefreshedAtForID(kb.ConvertKey(key), time.Now())
	}

	return lookupRes.peers, ctx.Err()
}
