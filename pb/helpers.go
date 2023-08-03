package dht_pb

import (
	"errors"

	pb "github.com/ipfs/boxo/bitswap/message/pb"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/plprobelab/go-kademlia/kad"
	"github.com/plprobelab/go-kademlia/key"
)

var ErrNoValidAddresses = errors.New("no valid addresses")

var (
	_ kad.Request[key.Key256, multiaddr.Multiaddr]  = (*pb.Message)(nil)
	_ kad.Response[key.Key256, multiaddr.Multiaddr] = (*pb.Message)(nil)
)

func (m *Message) Target() key.Key256 {
	p, err := peer.IDFromBytes(m.GetKey())
	if err != nil {
		return key.ZeroKey256()
	}
	return key.NewKey256([]byte(p.String()))
}

func (m *Message) EmptyResponse() kad.Response[key.Key256, multiaddr.Multiaddr] {
	return &Message{}
}

func (m *Message) CloserNodes() []kad.NodeInfo[key.Key256, multiaddr.Multiaddr] {
	closerPeers := m.GetCloserPeers()
	if closerPeers == nil {
		return []kad.NodeInfo[key.Key256, multiaddr.Multiaddr]{}
	}
	return ParsePeers(closerPeers)
}

func ParsePeers(pbps []*Message_Peer) []kad.NodeInfo[key.Key256, multiaddr.Multiaddr] {
	peers := make([]kad.NodeInfo[key.Key256, multiaddr.Multiaddr], 0, len(pbps))
	for _, p := range pbps {
		pi, err := PBPeerToPeerInfo(p)
		if err == nil {
			peers = append(peers, pi)
		}
	}
	return peers
}

func PBPeerToPeerInfo(pbp *Message_Peer) (*peer.AddrInfo, error) {
	addrs := make([]multiaddr.Multiaddr, 0, len(pbp.Addrs))
	for _, a := range pbp.Addrs {
		addr, err := multiaddr.NewMultiaddrBytes(a)
		if err == nil {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return nil, ErrNoValidAddresses
	}

	return kad.NewAddrInfo(peer.AddrInfo{
		ID:    peer.ID(pbp.Id),
		Addrs: addrs,
	}), nil
}
