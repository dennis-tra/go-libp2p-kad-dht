package dht

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"gonum.org/v1/gonum/stat"

	ks "github.com/whyrusleeping/go-keyspace"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
)

var keyspaceMax, _ = new(big.Int).SetString(strings.Repeat("F", 64), 16)

type AddProviderRPCState int

const (
	Sent AddProviderRPCState = iota + 1
	Success
	Failure
)

type addProviderState struct {
	peerStatesLk sync.RWMutex
	peerStates   map[peer.ID]AddProviderRPCState
}

// GetClosestPeersEstimator .
func (dht *IpfsDHT) GetClosestPeersEstimator(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("can't lookup empty key")
	}

	netSize, _, _, _ := dht.rtRefreshManager.NetworkSize()

	aps := addProviderState{
		peerStates: map[peer.ID]AddProviderRPCState{},
	}
	var wg sync.WaitGroup
	kskey := ks.XORKeySpace.Key([]byte(key))

	lookupRes, err := dht.runLookupWithFollowup(ctx, key,
		func(ctx context.Context, p peer.ID) ([]*peer.AddrInfo, error) {
			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type: routing.SendingQuery,
				ID:   p,
			})

			peers, err := dht.protoMessenger.GetClosestPeers(ctx, p, peer.ID(key))
			if err != nil {
				routing.PublishQueryEvent(ctx, &routing.QueryEvent{
					Type:  routing.QueryError,
					ID:    p,
					Extra: err.Error(),
				})
				logger.Debugf("error getting closer peers: %s", err)
				return nil, err
			}

			// For DHT query command
			routing.PublishQueryEvent(ctx, &routing.QueryEvent{
				Type:      routing.PeerResponse,
				ID:        p,
				Responses: peers,
			})

			return peers, err
		},
		func(qps *qpeerset.QueryPeerset) bool {
			closest := qps.GetClosestInStates(qpeerset.PeerHeard, qpeerset.PeerWaiting, qpeerset.PeerQueried)

			aps.peerStatesLk.Lock()
			defer aps.peerStatesLk.Unlock()

			for i, p := range closest {

				if _, found := aps.peerStates[p]; found {
					continue
				}

				fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
				normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()

				threshold := (float64(i) + 1) / (netSize + 1)
				if normedDist > threshold {
					break // closest is ordered, so we can break here
				}

				wg.Add(1)
				go func(p2 peer.ID) {
					err := dht.protoMessenger.PutProvider(ctx, p2, []byte(key), dht.host)
					aps.peerStatesLk.Lock()
					if err != nil {
						aps.peerStates[p] = Failure
					} else {
						aps.peerStates[p] = Success
					}
					aps.peerStatesLk.Unlock()
					wg.Done()
				}(p)

				aps.peerStates[p] = Sent
			}

			shouldStop := false

			waitingAndSuccessCount := 0
			for _, s := range aps.peerStates {
				if s == Sent || s == Success {
					waitingAndSuccessCount += 1
				}
			}

			if waitingAndSuccessCount >= dht.bucketSize {
				fmt.Printf("Stopping due to waitingAndSuccessCount %d >= %d\n", waitingAndSuccessCount, dht.bucketSize)
				shouldStop = true
			}

			closestDistances := []float64{}

			for i, p := range closest {
				fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
				normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()
				closestDistances = append(closestDistances, normedDist)
				if i >= dht.bucketSize {
					break
				}
			}

			mean := stat.Mean(closestDistances, nil)
			if mean < float64(dht.bucketSize)/(netSize+1) {
				fmt.Printf("Stopping due to mean %f < %f\n", mean, float64(dht.bucketSize)/(netSize+1))
				shouldStop = true
			}

			return shouldStop
		},
	)
	if err != nil {
		return err
	}

	aps.peerStatesLk.RLock()
	for _, p := range lookupRes.peers {
		if _, found := aps.peerStates[p]; found {
			continue
		}

		fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
		normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()

		wg.Add(1)
		go func(p2 peer.ID) {
			fmt.Println("Putting Provider Record to", p2.String(), normedDist)
			if err = dht.protoMessenger.PutProvider(ctx, p2, []byte(key), dht.host); err != nil {
				fmt.Println("error put provider", err)
			}
			wg.Done()
		}(p)
	}
	aps.peerStatesLk.RUnlock()

	wg.Wait()

	return ctx.Err()
}
