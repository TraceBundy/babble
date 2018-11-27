package node

import (
	"math/rand"

	"github.com/mosaicnetworks/babble/src/peers"
)

//XXX PeerSelector needs major refactoring

type PeerSelector interface {
	Peers() *peers.PeerSet
	UpdateLast(peer uint32)
	Next() *peers.Peer
}

//+++++++++++++++++++++++++++++++++++++++
//RANDOM

type RandomPeerSelector struct {
	peers  *peers.PeerSet
	selfID uint32
	last   uint32
}

func NewRandomPeerSelector(peerSet *peers.PeerSet, selfID uint32) *RandomPeerSelector {
	return &RandomPeerSelector{
		selfID: selfID,
		peers:  peerSet,
	}
}

func (ps *RandomPeerSelector) Peers() *peers.PeerSet {
	return ps.peers
}

func (ps *RandomPeerSelector) UpdateLast(peer uint32) {
	ps.last = peer
}

func (ps *RandomPeerSelector) Next() *peers.Peer {
	selectablePeers := ps.peers.Peers

	_, selectablePeers = peers.ExcludePeer(selectablePeers, ps.selfID)

	if len(selectablePeers) > 1 {
		_, selectablePeers = peers.ExcludePeer(selectablePeers, ps.last)
	}

	if len(selectablePeers) == 0 {
		return nil
	}

	i := rand.Intn(len(selectablePeers))

	peer := selectablePeers[i]

	return peer
}
