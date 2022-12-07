package dht

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p-kad-dht/netsize"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	"github.com/libp2p/go-libp2p/core/peer"
	ks "github.com/whyrusleeping/go-keyspace"
	"gonum.org/v1/gonum/mathext"
)

const (
	// OptProvIndividualThresholdCertainty describes how sure we want to be that an individual peer that
	// we find during walking the DHT actually belongs to the k-closest peers based on the current network size
	// estimate.
	OptProvIndividualThresholdCertainty = 0.9

	// OptProvSetThresholdStrictness describes the probability that the set of closest peers is actually further
	// away then the calculated set threshold. Put differently, what is the probability that we are too strict and
	// don't terminate the process early because we can't find any closer peers.
	OptProvSetThresholdStrictness = 0.1

	// OptProvReturnRatio corresponds to how many ADD_PROVIDER RPCs must have completed (regardless of success)
	// before we return to the user. The ratio of 0.75 equals 15 RPC as it is based on the Kademlia bucket size.
	OptProvReturnRatio = 0.75
)

type addProviderRPCState int

const (
	Sent addProviderRPCState = iota + 1
	Success
	Failure
)

type estimatorState struct {
	// context for all ADD_PROVIDER RPCs
	putCtx context.Context

	// reference to the DHT
	dht *IpfsDHT

	// the most recent network size estimation
	networkSize float64

	// a channel indicating when an ADD_PROVIDER RPC completed (successful or not)
	doneChan chan struct{}

	// tracks which peers we have stored the provider records with
	peerStatesLk sync.RWMutex
	peerStates   map[peer.ID]addProviderRPCState

	// the key to provide
	key string

	// the key to provide transformed into the Kademlia key space
	ksKey ks.Key

	// distance threshold for individual peers. If peers are closer than this number we store
	// the provider records right away.
	individualThreshold float64

	// distance threshold for the set of bucketSize closest peers. If the average distance of the bucketSize
	// closest peers is below this number we stop the DHT walk and store the remaining provider records.
	// "remaining" because we have likely already stored some on peers that were below the individualThreshold.
	setThreshold float64

	// number of completed (regardless of success) ADD_PROVIDER RPCs before we return control back to the user.
	returnThreshold int

	// putProvDone counts the ADD_PROVIDER RPCs that have completed (successful and unsuccessful)
	putProvDone atomic.Int32
}

func (dht *IpfsDHT) newEstimatorState(ctx context.Context, key string) (*estimatorState, error) {
	// get network size and err out if there is no reasonable estimate
	networkSize, err := dht.nsEstimator.NetworkSize()
	if err != nil {
		return nil, err
	}

	individualThreshold := mathext.GammaIncRegInv(float64(dht.bucketSize), 1-OptProvIndividualThresholdCertainty) / networkSize
	setThreshold := mathext.GammaIncRegInv(float64(dht.bucketSize)/2.0+1, 1-OptProvSetThresholdStrictness) / networkSize
	returnThreshold := int(math.Ceil(float64(dht.bucketSize) * OptProvReturnRatio))

	return &estimatorState{
		putCtx:              ctx,
		dht:                 dht,
		key:                 key,
		doneChan:            make(chan struct{}, returnThreshold), // buffered channel to not miss events
		ksKey:               ks.XORKeySpace.Key([]byte(key)),
		networkSize:         networkSize,
		peerStates:          map[peer.ID]addProviderRPCState{},
		individualThreshold: individualThreshold,
		setThreshold:        setThreshold,
		returnThreshold:     returnThreshold,
		putProvDone:         atomic.Int32{},
	}, nil
}

func (dht *IpfsDHT) GetAndProvideToClosestPeers(outerCtx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("can't lookup empty key")
	}

	// initialize new context for all putProvider operations.
	// We don't want to give the outer context to the put operations as we return early before all
	// put operations have finished to avoid the long tail of the latency distribution. If we
	// provided the outer context the put operations may be cancelled depending on what happens
	// with the context on the user side.
	putCtx, putCtxCancel := context.WithTimeout(context.Background(), time.Minute)

	es, err := dht.newEstimatorState(putCtx, key)
	if err != nil {
		putCtxCancel()
		return err
	}

	// initialize context that finishes when this function returns
	innerCtx, innerCtxCancel := context.WithCancel(outerCtx)
	defer innerCtxCancel()

	go func() {
		select {
		case <-outerCtx.Done():
			// If the outer context gets cancelled while we're still in this function. We stop all
			// pending put operations.
			putCtxCancel()
		case <-innerCtx.Done():
			// We have returned from this function. Ignore cancellations of the outer context and continue
			// with the remaining put operations.
		}
	}()

	lookupRes, err := dht.runLookupWithFollowup(outerCtx, key, dht.pmGetClosestPeers(key), es.stopFn)
	if err != nil {
		return err
	}

	// Store the provider records with all of the closest peers
	// we haven't already contacted.
	es.peerStatesLk.Lock()
	for _, p := range lookupRes.peers {
		if _, found := es.peerStates[p]; found {
			continue
		}

		go es.putProviderRecord(p)
		es.peerStates[p] = Sent
	}
	es.peerStatesLk.Unlock()

	// wait until a threshold number of RPCs have completed
	es.waitForRPCs()

	return outerCtx.Err()
}

func (es *estimatorState) stopFn(qps *qpeerset.QueryPeerset) bool {
	es.peerStatesLk.Lock()
	defer es.peerStatesLk.Unlock()

	// get currently known closest peers and check if any of them is already very close.
	// If so -> store provider records straight away.
	closest := qps.GetClosestNInStates(es.dht.bucketSize, qpeerset.PeerHeard, qpeerset.PeerWaiting, qpeerset.PeerQueried)
	distances := make([]float64, es.dht.bucketSize)
	for i, p := range closest {
		// calculate distance of peer p to the target key
		distances[i] = netsize.NormedDistance(p, es.ksKey)

		// Check if we have already interacted with that peer
		if _, found := es.peerStates[p]; found {
			continue
		}

		// Check if peer is close enough to store the provider record with
		if distances[i] > es.individualThreshold {
			continue
		}

		// peer is indeed very close already -> store the provider record directly with it!
		go es.putProviderRecord(p)

		// keep state that we've contacted that peer
		es.peerStates[p] = Sent
	}

	// count number of peers we have already (successfully) contacted via the above method
	sentAndSuccessCount := 0
	for _, s := range es.peerStates {
		if s == Sent || s == Success {
			sentAndSuccessCount += 1
		}
	}

	// if we have already contacted more than bucketSize peers stop the procedure
	if sentAndSuccessCount >= es.dht.bucketSize {
		return true
	}

	// calculate average distance of the set of closest peers
	sum := 0.0
	for _, d := range distances {
		sum += d
	}
	avg := sum / float64(len(distances))

	// if the average is below the set threshold stop the procedure
	return avg < es.setThreshold
}

func (es *estimatorState) putProviderRecord(pid peer.ID) {
	err := es.dht.protoMessenger.PutProvider(es.putCtx, pid, []byte(es.key), es.dht.host)
	es.peerStatesLk.Lock()
	if err != nil {
		es.peerStates[pid] = Failure
	} else {
		es.peerStates[pid] = Success
	}
	es.peerStatesLk.Unlock()

	// indicate that this ADD_PROVIDER RPC has completed
	es.doneChan <- struct{}{}
}

// waitForRPCs waits for a subset of ADD_PROVIDER RPCs to complete and then acquire a lease on
// a bound channel to return early back to the user and prevent unbound asynchronicity. If
// there are already too many requests in-flight we are just waiting for our current set to
// finish.
func (es *estimatorState) waitForRPCs() {
	es.peerStatesLk.RLock()
	rpcCount := len(es.peerStates)
	es.peerStatesLk.RUnlock()

	// returnThreshold can't be larger than the total number issued RPCs
	if es.returnThreshold > rpcCount {
		es.returnThreshold = rpcCount
	}

	// Wait until returnThreshold ADD_PROVIDER RPCs have returned
	for range es.doneChan {
		if int(es.putProvDone.Add(1)) == es.returnThreshold {
			break
		}
	}
	// At this point only a subset of all ADD_PROVIDER RPCs have completed.
	// We want to give control back to the user as soon as possible because
	// it is highly likely that at least one of the remaining RPCs will time
	// out and thus slow down the whole processes. The provider records will
	// already be available with less than the total number of RPCs having
	// finished. This has been investigated here:
	// https://github.com/protocol/network-measurements/blob/master/results/rfm17-provider-record-liveness.md

	// For the remaining ADD_PROVIDER RPCs try to acquire a lease on the optProvJobsPool channel.
	// If that worked we need to consume the doneChan and release the acquired lease on the
	// optProvJobsPool channel.
	remaining := rpcCount - int(es.putProvDone.Load())
	for i := 0; i < remaining; i++ {
		select {
		case es.dht.optProvJobsPool <- struct{}{}:
			// We were able to acquire a lease on the optProvJobsPool channel.
			// Consume doneChan to release the acquired lease again.
			go es.consumeDoneChan(rpcCount)
		case <-es.doneChan:
			// We were not able to acquire a lease but an ADD_PROVIDER RPC resolved.
			if int(es.putProvDone.Add(1)) == rpcCount {
				close(es.doneChan)
			}
		}
	}
}

func (es *estimatorState) consumeDoneChan(until int) {
	// Wait for an RPC to finish
	<-es.doneChan

	// Release acquired lease for other's to get a spot
	<-es.dht.optProvJobsPool

	// If all RPCs have finished, close the channel.
	if int(es.putProvDone.Add(1)) == until {
		close(es.doneChan)
	}
}
