package dht

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-kad-dht/netsize"
	"github.com/libp2p/go-libp2p-kad-dht/qpeerset"
	kb "github.com/libp2p/go-libp2p-kbucket"
	log "github.com/sirupsen/logrus"
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

	//Functionality to keep track of the cid and the providers
	nonHashedKey    cid.Cid
	cidAndProviders map[string][]*peer.AddrInfo
	mu              sync.Mutex
	addProviderWG   sync.WaitGroup
	//send an empty struct after the ADD providers RPC
	//have been finished
	doneProviderChannel chan struct{}

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
}
type CidAndProvider struct {
	CID         string
	AddressInfo *peer.AddrInfo
}

func (dht *IpfsDHT) newEstimatorState(ctx context.Context, key string, nonHashedKey cid.Cid) (*estimatorState, error) {
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
		nonHashedKey:        nonHashedKey,
		doneChan:            make(chan struct{}, returnThreshold), // buffered channel to not miss events
		cidAndProviders:     make(map[string][]*peer.AddrInfo),
		ksKey:               ks.XORKeySpace.Key([]byte(key)),
		networkSize:         networkSize,
		peerStates:          map[peer.ID]addProviderRPCState{},
		individualThreshold: individualThreshold,
		setThreshold:        setThreshold,
		returnThreshold:     returnThreshold,
	}, nil
}

func (dht *IpfsDHT) GetAndProvideToClosestPeers(outerCtx context.Context, key string, nonHashedKey cid.Cid) error {
	if key == "" {
		return fmt.Errorf("can't lookup empty key")
	}

	// initialize new context for all putProvider operations.
	// We don't want to give the outer context to the put operations as we return early before all
	// put operations have finished to avoid the long tail of the latency distribution. If we
	// provided the outer context the put operations may be cancelled depending on what happens
	// with the context on the user side.
	putCtx, putCtxCancel := context.WithTimeout(context.Background(), time.Minute)

	log.Debug("trying to create new estimator state")

	es, err := dht.newEstimatorState(putCtx, key, nonHashedKey)
	if err != nil {
		//stop the running provide
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
		//for the remaining five operations
		case <-innerCtx.Done():
			// We have returned from this function. Ignore cancellations of the outer context and continue
			// with the remaining put operations.
		}
	}()

	lookupRes, err := dht.runLookupWithFollowup(outerCtx, key, dht.pmGetClosestPeers(key), es.stopFn)
	if err != nil {
		return err
	}

	log.Debug("done run lookup with followup")

	// Store the provider records with all of the closest peers
	// we haven't already contacted.
	//TODO needs some modification by to be able to get all of the provider records.
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
	es.waitForRPCs(es.returnThreshold)
	es.addProviderWG.Wait()
	var savetofilesWG sync.WaitGroup
	go es.saveToFiles(&savetofilesWG)
	savetofilesWG.Wait()
	log.Debug("done waiting for rpcs")
	if outerCtx.Err() == nil && lookupRes.completed { // likely the "completed" field is false but that's not a given

		// tracking lookup results for network size estimator as "completed" is true
		if err = dht.nsEstimator.Track(key, lookupRes.closest); err != nil {
			logger.Warnf("network size estimator track peers: %s", err)
		}

		// refresh the cpl for this key as the query was successful
		dht.routingTable.ResetCplRefreshedAtForID(kb.ConvertKey(key), time.Now())
	}

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
		log.Debug("failure while adding provider")
		es.peerStates[pid] = Failure
	} else {
		log.Debug("success while adding provider")
		es.peerStates[pid] = Success
		//if the peer is successfully inserted into the DHT send it to the channel to be inserted into the map
		providerRecord := &peer.AddrInfo{
			ID:    pid,
			Addrs: es.dht.host.Peerstore().Addrs(pid),
		}
		cidAndProvider := &CidAndProvider{
			CID:         es.nonHashedKey.String(),
			AddressInfo: providerRecord,
		}
		go es.addNewProviderToMap(cidAndProvider, &es.addProviderWG)
	}
	es.peerStatesLk.Unlock()

	// indicate that this ADD_PROVIDER RPC has completed
	es.doneChan <- struct{}{}
	log.Debug("sent empty struct over done channel")
}

//Insert the new providers received bty the putProviderRecord method into the corresponding map inside the state.
func (es *estimatorState) addNewProviderToMap(pr *CidAndProvider, addproviderWG *sync.WaitGroup) {
	addproviderWG.Add(1)
	defer addproviderWG.Done()
	es.mu.Lock()
	log.Debugf("trying to add cid: %s and provider %s to map", pr.CID, pr.AddressInfo.ID.String())
	if addressinfo, ok := es.cidAndProviders[pr.CID]; ok {
		log.Debug("added address info for provider")
		es.cidAndProviders[pr.CID] = append(addressinfo, pr.AddressInfo)
	} else {
		log.Debug("initialized map for cids")
		addressinfo = append(addressinfo, pr.AddressInfo)
		es.cidAndProviders[pr.CID] = addressinfo
	}
	es.mu.Unlock()
}

func (es *estimatorState) waitForRPCs(returnThreshold int) {
	es.peerStatesLk.RLock()
	rpcCount := len(es.peerStates)
	es.peerStatesLk.RUnlock()

	// returnThreshold can't be larger than the total number issued RPCs
	if returnThreshold > rpcCount {
		returnThreshold = rpcCount
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		dones := 0
		for range es.doneChan {
			dones += 1
			// Indicate to the wait group that returnThreshold RPCs have finished but
			// don't break here. Keep go-routine around so that the putProviderRecord
			// go-routines can write to the done channel and everything gets cleaned
			// up properly.
			if dones == returnThreshold {
				wg.Done()
			}

			// If the total number RPCs was reached break for loop and close the done channel.
			if dones == rpcCount {
				break
			}
		}
		close(es.doneChan)
		log.Debug("closed done channel")
	}()

	// wait until returnThreshold ADD_PROVIDER RPCs have finished
	wg.Wait()
}

//A container for the encapsulated struct.
//
//File containts a json array of provider records.
//[{ProviderRecord1},{ProviderRecord2},{ProviderRecord3}]
type ProviderRecords struct {
	EncapsulatedJSONProviderRecords []EncapsulatedJSONProviderRecord `json:"ProviderRecords"`
}

//This struct will be used to create,read and store the encapsulated data necessary for reading the
//provider records.
type EncapsulatedJSONProviderRecord struct {
	ID        string   `json:"PeerID"`
	CID       string   `json:"ContentID"`
	Addresses []string `json:"PeerMultiaddresses"`
}

//Creates a new:
//	EncapsulatedCidProvider struct {
//		ID      string
//		CID     string
//		Address ma.Multiaddr
//	}
func NewEncapsulatedJSONCidProvider(id string, cid string, addresses []string) EncapsulatedJSONProviderRecord {
	return EncapsulatedJSONProviderRecord{
		ID:        id,
		CID:       cid,
		Addresses: addresses,
	}
}

const filename = "C:\\Users\\fotis\\GolandProjects\\retrieval-success-rate\\go-libp2p-kad-dht\\providers.json"

//Saves the providers along with the CIDs in a json format. In an error occurs it returns the error or else
//it returns nil.
//
//Because we want to add a new provider record in the file for each new provider record
//we need to read the contents and add the new provider record to the already existing array.
func saveProvidersSimpleJSONFile(filename string, contentID string, addressInfos []*peer.AddrInfo) error {
	log.Debug("starting to save providers to file")
	jsonFile, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return errors.Wrap(err, "while trying to open json file")
	}
	defer func(jsonFile *os.File) {
		err := jsonFile.Close()
		if err != nil {
			log.Errorf("error %s while closing down providers file", err)
		}
	}(jsonFile)

	//create a new instance of ProviderRecords struct which is a container for the encapsulated struct
	var records ProviderRecords

	bytes, err := io.ReadAll(jsonFile)
	if err != nil {
		return errors.Wrap(err, "while trying to read json file")
	}

	if len(bytes) != 0 {
		//read the existing data. Will throw error if they are not of type EncapsulatedJSONproviderRecord
		err = json.Unmarshal(bytes, &records)
		if err != nil {
			return errors.Wrap(err, "while unmarshalling json")
		}
	}

	for _, addressInfo := range addressInfos {

		if addressInfo == nil {
			continue
		}

		stringaddrss := make([]string, 0)
		for _, addrss := range addressInfo.Addrs {
			stringaddrss = append(stringaddrss, addrss.String())
		}

		//create a new encapsulated struct
		NewEncapsulatedJSONProviderRecord := NewEncapsulatedJSONCidProvider(addressInfo.ID.String(), contentID, stringaddrss)
		log.Debugf("Created new encapsulated JSON provider record: ID:%s,CID:%s,Addresses:%v", NewEncapsulatedJSONProviderRecord.ID, NewEncapsulatedJSONProviderRecord.CID, NewEncapsulatedJSONProviderRecord.Addresses)
		//insert the new provider record to the slice in memory containing the provider records read
		records.EncapsulatedJSONProviderRecords = append(records.EncapsulatedJSONProviderRecords, NewEncapsulatedJSONProviderRecord)
	}
	data, err := json.MarshalIndent(&records, "", " ")
	if err != nil {
		return errors.Wrap(err, "while marshalling json data")
	}
	err = os.WriteFile(filename, data, 0644)
	if err != nil {
		return errors.Wrap(err, "while trying to write json data to file")
	}
	return nil
}

func saveProvidersToEncodedJSONFile(filename string, contentID string, addressInfos []*peer.AddrInfo) error {
	log.Debug("starting to save providers to file")
	jsonFile, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
	if err != nil {
		return errors.Wrap(err, "while trying to open json file")
	}
	defer func(jsonFile *os.File) {
		err := jsonFile.Close()
		if err != nil {
			log.Errorf("error %s while closing down providers file", err)
		}
	}(jsonFile)

	//create a new instance of ProviderRecords struct which is a container for the encapsulated struct
	var records ProviderRecords

	encoder := json.NewEncoder(jsonFile)

	for _, addressInfo := range addressInfos {
		if addressInfo == nil {
			continue
		}
		stringaddrss := make([]string, 0)
		for _, addrss := range addressInfo.Addrs {
			stringaddrss = append(stringaddrss, addrss.String())
		}

		//create a new encapsulated struct
		NewEncapsulatedJSONProviderRecord := NewEncapsulatedJSONCidProvider(addressInfo.ID.String(), contentID, stringaddrss)
		log.Debugf("Created new encapsulated JSON provider record: ID:%s,CID:%s,Addresses:%v", NewEncapsulatedJSONProviderRecord.ID, NewEncapsulatedJSONProviderRecord.CID, NewEncapsulatedJSONProviderRecord.Addresses)
		//insert the new provider record to the slice in memory containing the provider records read
		records.EncapsulatedJSONProviderRecords = append(records.EncapsulatedJSONProviderRecords, NewEncapsulatedJSONProviderRecord)
	}
	encoder.Encode(&records)
	return nil
}

func saveProvidersToMultipleEncodedJSONFiles(filename string, contentID string, addressInfos []*peer.AddrInfo) error {
	newf := fmt.Sprintf("encoded" + contentID + filename)
	err := saveProvidersToEncodedJSONFile(newf, contentID, addressInfos)
	if err != nil {
		return errors.Wrap(err, "while trying to save to multiple json files")
	}
	return nil
}

func saveProvidersToMultipleSimpleJSONFiles(filename string, contentID string, addressInfos []*peer.AddrInfo) error {
	newf := fmt.Sprintf(contentID + filename)
	err := saveProvidersSimpleJSONFile(newf, contentID, addressInfos)
	if err != nil {
		return errors.Wrap(err, "while trying to save to multiple json files")
	}
	return nil
}

func (es estimatorState) saveToFiles(savetofilesWG *sync.WaitGroup) {
	savetofilesWG.Add(1)
	defer savetofilesWG.Done()
	for ncid, prs := range es.cidAndProviders {
		err := saveProvidersSimpleJSONFile("providers.json", ncid, prs)
		if err != nil {
			log.Errorf("error: %s", err)
		}
		err = saveProvidersToMultipleSimpleJSONFiles("providers.json", ncid, prs)
		if err != nil {
			log.Errorf("error: %s", err)
		}
		err = saveProvidersToEncodedJSONFile("providersen.json", ncid, prs)
		if err != nil {
			log.Errorf("error: %s", err)
		}
		err = saveProvidersToMultipleEncodedJSONFiles("providers.json", ncid, prs)
		if err != nil {
			log.Errorf("error: %s", err)
		}
	}
}
