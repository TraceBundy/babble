package net

import (
	"github.com/mosaicnetworks/babble/src/hashgraph"
	"github.com/mosaicnetworks/babble/src/peers"
)

type SyncRequest struct {
	FromID uint32
	Known  map[uint32]int
}

type SyncResponse struct {
	FromID    uint32
	SyncLimit bool
	Events    []hashgraph.WireEvent
	Known     map[uint32]int
}

//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++

type JoinRequest struct {
	FromID uint32
	Peer   peers.Peer // peer that want to join
}

type JoinResponse struct {
	FromID uint32
	Peer   peers.Peer // peer to fastforward from
}

//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++

type EagerSyncRequest struct {
	FromID uint32
	Events []hashgraph.WireEvent
}

type EagerSyncResponse struct {
	FromID  uint32
	Success bool
}

//++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++

type FastForwardRequest struct {
	FromID uint32
}

type FastForwardResponse struct {
	FromID   uint32
	Block    hashgraph.Block
	Frame    hashgraph.Frame
	Snapshot []byte
}
