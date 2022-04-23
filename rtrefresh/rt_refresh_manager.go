package rtrefresh

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"gonum.org/v1/gonum/mat"

	"gonum.org/v1/gonum/stat"

	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	ks "github.com/whyrusleeping/go-keyspace"
)

var (
	logger         = logging.Logger("dht/RtRefreshManager")
	keyspaceMax, _ = new(big.Int).SetString(strings.Repeat("F", 64), 16)
)

const (
	peerPingTimeout = 10 * time.Second
)

type triggerRefreshReq struct {
	respCh          chan error
	forceCplRefresh bool
}

type RtRefreshManager struct {
	ctx       context.Context
	cancel    context.CancelFunc
	refcount  sync.WaitGroup
	closeOnce sync.Once

	// peerId of this DHT peer i.e. self peerId.
	h         host.Host
	dhtPeerId peer.ID
	rt        *kbucket.RoutingTable

	enableAutoRefresh   bool                                                     // should run periodic refreshes ?
	refreshKeyGenFnc    func(cpl uint) (string, error)                           // generate the key for the query to refresh this cpl
	refreshQueryFnc     func(ctx context.Context, key string) ([]peer.ID, error) // query to run for a refresh.
	refreshQueryTimeout time.Duration                                            // timeout for one refresh query

	// interval between two periodic refreshes.
	// also, a cpl wont be refreshed if the time since it was last refreshed
	// is below the interval..unless a "forced" refresh is done.
	refreshInterval                    time.Duration
	successfulOutboundQueryGracePeriod time.Duration

	triggerRefresh chan *triggerRefreshReq // channel to write refresh requests to.

	refreshDoneCh chan struct{} // write to this channel after every refresh

	// Order statistic distances
	osDistancesLk sync.RWMutex
	osDistances   map[int][]float64
	osDistancesTs map[int][]time.Time
}

func NewRtRefreshManager(h host.Host, rt *kbucket.RoutingTable, autoRefresh bool,
	refreshKeyGenFnc func(cpl uint) (string, error),
	refreshQueryFnc func(ctx context.Context, key string) ([]peer.ID, error),
	refreshQueryTimeout time.Duration,
	refreshInterval time.Duration,
	successfulOutboundQueryGracePeriod time.Duration,
	refreshDoneCh chan struct{}) (*RtRefreshManager, error) {

	osDistances := map[int][]float64{}
	osDistancesTs := map[int][]time.Time{}
	for i := 0; i < 20; i++ { // TODO: take "20" from routing table?
		osDistances[i] = []float64{}
		osDistancesTs[i] = []time.Time{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &RtRefreshManager{
		ctx:       ctx,
		cancel:    cancel,
		h:         h,
		dhtPeerId: h.ID(),
		rt:        rt,

		enableAutoRefresh: autoRefresh,
		refreshKeyGenFnc:  refreshKeyGenFnc,
		refreshQueryFnc:   refreshQueryFnc,

		refreshQueryTimeout:                refreshQueryTimeout,
		refreshInterval:                    refreshInterval,
		successfulOutboundQueryGracePeriod: successfulOutboundQueryGracePeriod,

		triggerRefresh: make(chan *triggerRefreshReq),
		refreshDoneCh:  refreshDoneCh,

		osDistancesLk: sync.RWMutex{},
		osDistances:   osDistances,
		osDistancesTs: osDistancesTs,
	}, nil
}

func (r *RtRefreshManager) Start() error {
	r.refcount.Add(1)
	go r.loop()
	return nil
}

func (r *RtRefreshManager) Close() error {
	r.closeOnce.Do(func() {
		r.cancel()
		r.refcount.Wait()
	})
	return nil
}

// RefreshRoutingTable requests the refresh manager to refresh the Routing Table.
// If the force parameter is set to true true, all buckets will be refreshed irrespective of when they were last refreshed.
//
// The returned channel will block until the refresh finishes, then yield the
// error and close. The channel is buffered and safe to ignore.
func (r *RtRefreshManager) Refresh(force bool) <-chan error {
	resp := make(chan error, 1)
	r.refcount.Add(1)
	go func() {
		defer r.refcount.Done()
		select {
		case r.triggerRefresh <- &triggerRefreshReq{respCh: resp, forceCplRefresh: force}:
		case <-r.ctx.Done():
			resp <- r.ctx.Err()
		}
	}()

	return resp
}

// RefreshNoWait requests the refresh manager to refresh the Routing Table.
// However, it moves on without blocking if it's request can't get through.
func (r *RtRefreshManager) RefreshNoWait() {
	select {
	case r.triggerRefresh <- &triggerRefreshReq{}:
	default:
	}
}

func (r *RtRefreshManager) loop() {
	defer r.refcount.Done()

	var refreshTickrCh <-chan time.Time
	if r.enableAutoRefresh {
		err := r.doRefresh(true)
		if err != nil {
			logger.Warn("failed when refreshing routing table", err)
		}
		t := time.NewTicker(r.refreshInterval)
		defer t.Stop()
		refreshTickrCh = t.C
	}

	for {
		var waiting []chan<- error
		var forced bool
		select {
		case <-refreshTickrCh:
		case triggerRefreshReq := <-r.triggerRefresh:
			if triggerRefreshReq.respCh != nil {
				waiting = append(waiting, triggerRefreshReq.respCh)
			}
			forced = forced || triggerRefreshReq.forceCplRefresh
		case <-r.ctx.Done():
			return
		}

		// Batch multiple refresh requests if they're all waiting at the same time.
	OuterLoop:
		for {
			select {
			case triggerRefreshReq := <-r.triggerRefresh:
				if triggerRefreshReq.respCh != nil {
					waiting = append(waiting, triggerRefreshReq.respCh)
				}
				forced = forced || triggerRefreshReq.forceCplRefresh
			default:
				break OuterLoop
			}
		}

		// EXECUTE the refresh

		// ping Routing Table peers that haven't been heard of/from in the interval they should have been.
		// and evict them if they don't reply.
		var wg sync.WaitGroup
		for _, ps := range r.rt.GetPeerInfos() {
			if time.Since(ps.LastSuccessfulOutboundQueryAt) > r.successfulOutboundQueryGracePeriod {
				wg.Add(1)
				go func(ps kbucket.PeerInfo) {
					defer wg.Done()
					livelinessCtx, cancel := context.WithTimeout(r.ctx, peerPingTimeout)
					if err := r.h.Connect(livelinessCtx, peer.AddrInfo{ID: ps.Id}); err != nil {
						logger.Debugw("evicting peer after failed ping", "peer", ps.Id, "error", err)
						r.rt.RemovePeer(ps.Id)
					}
					cancel()
				}(ps)
			}
		}
		wg.Wait()

		// Query for self and refresh the required buckets
		err := r.doRefresh(forced)
		for _, w := range waiting {
			w <- err
			close(w)
		}
		if err != nil {
			logger.Warnw("failed when refreshing routing table", "error", err)
		}
	}
}

func (r *RtRefreshManager) doRefresh(forceRefresh bool) error {
	var merr error

	if err := r.queryForSelf(); err != nil {
		merr = multierror.Append(merr, err)
	}

	refreshCpls := r.rt.GetTrackedCplsForRefresh()

	rfnc := func(cpl uint) (err error) {
		if forceRefresh {
			err = r.refreshCpl(cpl)
		} else {
			err = r.refreshCplIfEligible(cpl, refreshCpls[cpl])
		}
		return
	}

	for c := range refreshCpls {
		cpl := uint(c)
		if err := rfnc(cpl); err != nil {
			merr = multierror.Append(merr, err)
		} else {
			// If we see a gap at a Cpl in the Routing table, we ONLY refresh up until the maximum cpl we
			// have in the Routing Table OR (2 * (Cpl+ 1) with the gap), whichever is smaller.
			// This is to prevent refreshes for Cpls that have no peers in the network but happen to be before a very high max Cpl
			// for which we do have peers in the network.
			// The number of 2 * (Cpl + 1) can be proved and a proof would have been written here if the programmer
			// had paid more attention in the Math classes at university.
			// So, please be patient and a doc explaining it will be published soon.
			if r.rt.NPeersForCpl(cpl) == 0 {
				lastCpl := min(2*(c+1), len(refreshCpls)-1)
				for i := c + 1; i < lastCpl+1; i++ {
					if err := rfnc(uint(i)); err != nil {
						merr = multierror.Append(merr, err)
					}
				}
				return merr
			}
		}
	}

	select {
	case r.refreshDoneCh <- struct{}{}:
	case <-r.ctx.Done():
		return r.ctx.Err()
	}

	return merr
}

func min(a int, b int) int {
	if a <= b {
		return a
	}

	return b
}

func (r *RtRefreshManager) refreshCplIfEligible(cpl uint, lastRefreshedAt time.Time) error {
	if time.Since(lastRefreshedAt) <= r.refreshInterval {
		logger.Debugf("not running refresh for cpl %d as time since last refresh not above interval", cpl)
		return nil
	}

	return r.refreshCpl(cpl)
}

func (r *RtRefreshManager) refreshCpl(cpl uint) error {
	// gen a key for the query to refresh the cpl
	key, err := r.refreshKeyGenFnc(cpl)
	if err != nil {
		return fmt.Errorf("failed to generated query key for cpl=%d, err=%s", cpl, err)
	}

	logger.Infof("starting refreshing cpl %d with key %s (routing table size was %d)",
		cpl, loggableRawKeyString(key), r.rt.Size())

	if err := r.runRefreshDHTQuery(key); err != nil {
		return fmt.Errorf("failed to refresh cpl=%d, err=%s", cpl, err)
	}

	logger.Infof("finished refreshing cpl %d, routing table size is now %d", cpl, r.rt.Size())
	return nil
}

func (r *RtRefreshManager) queryForSelf() error {
	if err := r.runRefreshDHTQuery(string(r.dhtPeerId)); err != nil {
		return fmt.Errorf("failed to query for self, err=%s", err)
	}
	return nil
}

func (r *RtRefreshManager) runRefreshDHTQuery(key string) error {
	queryCtx, cancel := context.WithTimeout(r.ctx, r.refreshQueryTimeout)
	defer cancel()

	peers, err := r.refreshQueryFnc(queryCtx, key)

	r.trackClosestPeersDistances(key, peers)
	r.PrintNetworkSize()

	if err == nil || (err == context.DeadlineExceeded && queryCtx.Err() == context.DeadlineExceeded) {
		return nil
	}

	return err
}

type osDistance struct {
	distance  float64
	timestamp time.Time
}

func (r *RtRefreshManager) trackClosestPeersDistances(key string, peers []peer.ID) {
	r.osDistancesLk.Lock()
	defer r.osDistancesLk.Unlock()

	msg := []string{}
	kskey := ks.XORKeySpace.Key([]byte(key))
	for i, p := range peers {
		fDist := new(big.Float).SetInt(ks.XORKeySpace.Key([]byte(p)).Distance(kskey))
		normedDist, _ := new(big.Float).Quo(fDist, new(big.Float).SetInt(keyspaceMax)).Float64()

		r.osDistances[i] = append(r.osDistances[i], normedDist)
		r.osDistancesTs[i] = append(r.osDistancesTs[i], time.Now())

		msg = append(msg, fmt.Sprintf("%.10f (%d)", normedDist, i+1))
	}
	fmt.Println(time.Now().Format(time.RFC3339Nano)+": New distances:", strings.Join(msg, ","))
}

type loggableRawKeyString string

func (lk loggableRawKeyString) String() string {
	k := string(lk)

	if len(k) == 0 {
		return k
	}

	encStr := base32.RawStdEncoding.EncodeToString([]byte(k))

	return encStr
}

func (r *RtRefreshManager) garbageCollectOsDistances() {
	r.osDistancesLk.Lock()
	defer r.osDistancesLk.Unlock()

	for i := 0; i < 20; i++ { // TODO: take "20" from routing table?
		for j, s := 0, len(r.osDistancesTs[i]); j < s; j++ {
			if time.Since(r.osDistancesTs[i][j]) < 2*time.Hour { // TODO: configurable
				continue
			}

			r.osDistances[i] = append(r.osDistances[i][:j], r.osDistances[i][j+1:]...)
			r.osDistancesTs[i] = append(r.osDistancesTs[i][:j], r.osDistancesTs[i][j+1:]...)
			s--
			j--
		}
	}
}

func (r *RtRefreshManager) NetworkSize() (float64, float64) {
	r.garbageCollectOsDistances()

	r.osDistancesLk.Lock()
	defer r.osDistancesLk.Unlock()

	xs := make([]float64, 20)
	ys := make([]float64, 20)
	yerrs := make([]float64, 20)
	sampleCount := 0

	for i := 0; i < 20; i++ {
		sampleCount += len(r.osDistances[i])
		y, yerr := stat.MeanStdDev(r.osDistances[i], nil)
		xs[i] = float64(i + 1)
		ys[i] = y
		yerrs[i] = yerr
	}

	beta, betaErr := linearFit(xs, ys, yerrs)
	r2 := stat.RSquared(xs, ys, nil, 0, beta)

	fmt.Printf("Linear Regression: beta: %.10f +- %.10f, r2: %.4f | Network: %.2f | 1σ: %.2f (%.1f%%) | 2σ: %.2f (%.1f%%) | 3σ: %.2f (%.1f%%) | Samples: %d\n",
		beta,
		betaErr,
		r2,
		1/beta-1,
		betaErr/(beta*beta),
		100*(betaErr/(beta*beta))/(1/beta-1),
		2*betaErr/(beta*beta),
		100*(2*betaErr/(beta*beta))/(1/beta-1),
		3*betaErr/(beta*beta),
		100*(3*betaErr/(beta*beta))/(1/beta-1),
		sampleCount,
	)

	return 1/beta - 1, betaErr / (beta * beta)
}

func linearFit(xs []float64, ys []float64, yerrs []float64) (float64, float64) {
	weights := make([]float64, len(yerrs))
	for i, yerr := range yerrs {
		weights[i] = 1 / yerr
	}

	_, beta := stat.LinearRegression(xs, ys, weights, true)

	vanderMat := vandermonde(xs, 1)
	weightMat := mat.NewDense(2, len(xs), append(weights, weights...))

	vanderMat.MulElem(vanderMat, weightMat.T())

	// intermediate scale
	var iscale mat.Dense
	iscale.MulElem(vanderMat, vanderMat)

	row := []float64{
		math.Sqrt(mat.Sum(iscale.ColView(0))),
		math.Sqrt(mat.Sum(iscale.ColView(1))),
	}
	scales := []float64{}
	for range xs {
		scales = append(scales, row...)
	}

	scale := mat.NewDense(len(xs), 2, scales)
	vanderMat.DivElem(vanderMat, scale)

	var c mat.Dense
	c.Mul(vanderMat.T(), vanderMat)

	var i mat.Dense
	err := i.Inverse(&c)
	if err != nil {
		panic(err)
	}

	outer := mat.NewDense(2, 2, []float64{
		scale.At(0, 0) * scale.At(0, 0), scale.At(0, 0) * scale.At(0, 1),
		scale.At(1, 0) * scale.At(0, 0), scale.At(1, 0) * scale.At(0, 1),
	})

	i.DivElem(&i, outer)

	return beta, math.Sqrt(i.At(0, 0))
}

// vandermonde constructs a matrix of the following shape:
// a = 4.5, 7.7, 2.1
// degree = 1
// 4.5 1
// 7.7 1
// 2.1 1
func vandermonde(a []float64, degree int) *mat.Dense {
	d := degree + 1
	x := mat.NewDense(len(a), d, nil)
	for i := range a {
		for j, p := d-1, 1.; j >= 0; j, p = j-1, p*a[i] {
			x.Set(i, j, p)
		}
	}
	return x
}
