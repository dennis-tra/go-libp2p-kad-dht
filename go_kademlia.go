package dht

import (
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"
	mhreg "github.com/multiformats/go-multihash/core"

	"github.com/plprobelab/go-kademlia/kad"
	"github.com/plprobelab/go-kademlia/key"
)

type peerID struct {
	peer.ID
}

var _ kad.NodeID[key.Key256] = (*peerID)(nil)

func newPeerID(p peer.ID) *peerID {
	return &peerID{p}
}

func (id peerID) Key() key.Key256 {
	hasher, _ := mhreg.GetHasher(mh.SHA2_256)
	hasher.Write([]byte(id.ID))
	return key.NewKey256(hasher.Sum(nil))
}

func (id peerID) NodeID() kad.NodeID[key.Key256] {
	return &id
}

func (id peerID) String() string {
	return id.ID.String()
}

type addrInfo struct {
	peer.AddrInfo
}

var _ kad.NodeInfo[key.Key256, multiaddr.Multiaddr] = (*addrInfo)(nil)

func newAddrInfo(ai peer.AddrInfo) *addrInfo {
	return &addrInfo{
		AddrInfo: ai,
	}
}

func (ai addrInfo) Key() key.Key256 {
	return newPeerID(ai.AddrInfo.ID).Key()
}

func (ai addrInfo) ID() kad.NodeID[key.Key256] {
	return newPeerID(ai.AddrInfo.ID)
}

func (ai addrInfo) Addresses() []multiaddr.Multiaddr {
	addrs := make([]multiaddr.Multiaddr, len(ai.Addrs))
	copy(addrs, ai.Addrs)
	return addrs
}

func (ai addrInfo) String() string {
	return ai.AddrInfo.ID.String()
}
