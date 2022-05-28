package dht

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	ks "github.com/whyrusleeping/go-keyspace"
	"gonum.org/v1/gonum/stat"
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

	netSize, _, _, _, _, _, _ := dht.rtRefreshManager.NetworkSize("", nil)

	aps := addProviderState{
		peerStates: map[peer.ID]AddProviderRPCState{},
	}

	var wg sync.WaitGroup
	kskey := ks.XORKeySpace.Key([]byte(key))

	putCtx, putCancel := context.WithCancel(ctx)

	wg.Add(1)
	doneChan := make(chan struct{})
	go func() {
		dones := 0
		for range doneChan {
			dones += 1
			if dones >= dht.bucketSize {
				break
			}
		}
		wg.Done()
	}()

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

			// routing table error in percent (stale routing table entries)
			rtErrPct := 5
			threshold := float64(dht.bucketSize) * (1 + float64(rtErrPct)/100)

			aps.peerStatesLk.Lock()
			defer aps.peerStatesLk.Unlock()

			for _, p := range closest {

				if _, found := aps.peerStates[p]; found {
					continue
				}

				fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
				normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()

				if normedDist > threshold {
					break // closest is ordered, so we can break here
				}

				go func(p2 peer.ID) {
					err := dht.protoMessenger.PutProvider(putCtx, p2, []byte(key), dht.host)
					aps.peerStatesLk.Lock()
					if err != nil {
						aps.peerStates[p] = Failure
					} else {
						aps.peerStates[p] = Success
					}
					aps.peerStatesLk.Unlock()

					select {
					case doneChan <- struct{}{}:
					default:
					}
				}(p)

				aps.peerStates[p] = Sent
			}

			waitingAndSuccessCount := 0
			for _, s := range aps.peerStates {
				if s == Sent || s == Success {
					waitingAndSuccessCount += 1
				}
			}

			if float64(waitingAndSuccessCount) >= threshold {
				fmt.Printf("Stopping due to waitingAndSuccessCount %d >= %d\n", waitingAndSuccessCount, dht.bucketSize)
				return true
			}

			closestDistances := []float64{}

			for i, p := range closest {
				fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
				normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()
				closestDistances = append(closestDistances, normedDist)
				if float64(i) >= threshold {
					break
				}
			}

			mean := stat.Mean(closestDistances, nil)
			if mean < threshold/(netSize+1) {
				fmt.Printf("Stopping due to mean %f < %f\n", mean, threshold/(netSize+1))
				return true
			}

			return false
		},
	)
	if err != nil {
		close(doneChan)
		putCancel()
		return err
	}

	aps.peerStatesLk.RLock()
	for _, p := range lookupRes.peers {
		if _, found := aps.peerStates[p]; found {
			continue
		}

		fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
		normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()

		go func(p2 peer.ID) {
			fmt.Println("Putting Provider Record to", p2.String(), normedDist)
			if err = dht.protoMessenger.PutProvider(putCtx, p2, []byte(key), dht.host); err != nil {
				fmt.Println("error put provider", err)
			}
			select {
			case doneChan <- struct{}{}:
			default:
			}
		}(p)
	}
	aps.peerStatesLk.RUnlock()

	wg.Wait()
	close(doneChan)
	putCancel()

	return ctx.Err()
}
