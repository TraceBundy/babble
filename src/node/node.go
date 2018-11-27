package node

import (
	"crypto/ecdsa"
	"fmt"
	"strconv"
	"sync"
	"time"

	hg "github.com/mosaicnetworks/babble/src/hashgraph"
	"github.com/mosaicnetworks/babble/src/net"
	"github.com/mosaicnetworks/babble/src/peers"
	"github.com/mosaicnetworks/babble/src/proxy"
	"github.com/sirupsen/logrus"
)

type Node struct {
	nodeState

	conf   *Config
	logger *logrus.Entry

	id       uint32
	core     *Core
	coreLock sync.Mutex

	trans net.Transport
	netCh <-chan net.RPC

	proxy            proxy.AppProxy
	submitCh         chan []byte
	submitInternalCh chan hg.InternalTransaction

	shutdownCh chan struct{}

	controlTimer *ControlTimer

	start        time.Time
	syncRequests int
	syncErrors   int

	needBoostrap bool
}

func NewNode(conf *Config,
	id uint32,
	key *ecdsa.PrivateKey,
	peers *peers.PeerSet,
	store hg.Store,
	trans net.Transport,
	proxy proxy.AppProxy,
) *Node {

	node := Node{
		id:               id,
		conf:             conf,
		logger:           conf.Logger.WithField("this_id", id),
		core:             NewCore(id, key, peers, store, proxy.CommitBlock, conf.Logger),
		trans:            trans,
		netCh:            trans.Consumer(),
		proxy:            proxy,
		submitCh:         proxy.SubmitCh(),
		submitInternalCh: proxy.SubmitInternalCh(),
		shutdownCh:       make(chan struct{}),
		controlTimer:     NewRandomControlTimer(),
	}

	node.needBoostrap = store.NeedBoostrap()

	//Initialize as Babbling
	node.setState(Babbling)

	return &node
}

func (n *Node) Init() error {
	if n.needBoostrap {
		n.logger.Debug("Bootstrap")

		if err := n.core.Bootstrap(); err != nil {
			return err
		}
	}

	return n.core.SetHeadAndSeq()
}

func (n *Node) connect(addr string) error {
	var res net.JoinResponse

	if len(addr) > 0 {
		response, err := n.requestJoin(addr)

		if err != nil {
			n.logger.Error("Cannot join:", addr, err)

			n.setState(Shutdown)

			return err
		}

		res = response
	}

	n.core.peers = n.core.peers.WithNewPeer(&res.Peer)
	n.core.peerSelector = NewRandomPeerSelector(n.core.peers, n.id)

	n.setState(CatchingUp)

	return nil
}

func (n *Node) RunAsync(addr string, gossip bool) {
	n.logger.Debug("runasync")

	go n.Run(addr, gossip)
}

func (n *Node) Run(addr string, gossip bool) {
	//The ControlTimer allows the background routines to control the
	//heartbeat timer when the node is in the Babbling state. The timer should
	//only be running when there are uncommitted transactions in the system.
	if len(addr) > 0 {
		n.setState(Joining)
	}

	go n.controlTimer.Run(n.conf.HeartbeatTimeout)

	//Execute some background work regardless of the state of the node.
	go n.doBackgroundWork()

	//Execute Node State Machine
	for {
		// Run different routines depending on node state
		state := n.getState()

		n.logger.WithField("state", state.String()).Debug("Run loop")

		switch state {
		case Babbling:
			n.babble(gossip)
		case CatchingUp:
			n.fastForward()
		case Joining:
			n.connect(addr)
		case Shutdown:
			return
		}
	}
}

func (n *Node) resetTimer() {
	n.coreLock.Lock()
	defer n.coreLock.Unlock()

	if !n.controlTimer.set {
		ts := n.conf.HeartbeatTimeout

		//Slow gossip if nothing interesting to say
		if n.core.hg.PendingLoadedEvents == 0 &&
			len(n.core.transactionPool) == 0 &&
			len(n.core.blockSignaturePool) == 0 {
			ts = time.Duration(time.Second)
		}

		n.controlTimer.resetCh <- ts
	}
}

func (n *Node) doBackgroundWork() {
	for {
		select {
		case t := <-n.submitCh:
			n.logger.Debug("Adding Transaction")
			n.addTransaction(t)
			n.resetTimer()
		case t := <-n.submitInternalCh:
			n.logger.Debug("Adding Internal Transaction")
			n.addInternalTransaction(t)
			n.resetTimer()
		case <-n.shutdownCh:
			return
		}
	}
}

//babble is interrupted when a gossip function, launched asychronously, changes
//the state from Babbling to CatchingUp, or when the node is shutdown.
//Otherwise, it processes RPC requests, periodicaly initiates gossip while there
//is something to gossip about, or waits.
func (n *Node) babble(gossip bool) {
	returnCh := make(chan struct{}, 100)
	for {
		select {
		case rpc := <-n.netCh:
			n.goFunc(func() {
				n.logger.Debug("Processing RPC")
				n.processRPC(rpc)
				n.resetTimer()
			})
		case <-n.controlTimer.tickCh:
			if gossip {
				n.logger.Debug("Time to gossip!")
				peer := n.core.peerSelector.Next()

				if peer == nil {
					n.logger.Debug("Waiting for peers...")

					continue
				}

				n.goFunc(func() { n.gossip(peer, returnCh) })
			}
			n.resetTimer()
		case <-returnCh:
			return
		case <-n.shutdownCh:
			return
		}
	}
}

func (n *Node) processRPC(rpc net.RPC) {
	switch cmd := rpc.Command.(type) {
	case *net.SyncRequest:
		n.processSyncRequest(rpc, cmd)
	case *net.JoinRequest:
		n.processJoinRequest(rpc, cmd)
	case *net.EagerSyncRequest:
		n.processEagerSyncRequest(rpc, cmd)
	case *net.FastForwardRequest:
		n.processFastForwardRequest(rpc, cmd)
	default:
		n.logger.WithField("cmd", rpc.Command).Error("Unexpected RPC command")
		rpc.Respond(nil, fmt.Errorf("unexpected command"))
	}
}

func (n *Node) processSyncRequest(rpc net.RPC, cmd *net.SyncRequest) {
	n.logger.WithFields(logrus.Fields{
		"from_id": cmd.FromID,
		"known":   cmd.Known,
	}).Debug("process SyncRequest")

	resp := &net.SyncResponse{
		FromID: n.id,
	}

	var respErr error

	//Check sync limit
	n.coreLock.Lock()

	overSyncLimit := n.core.OverSyncLimit(cmd.Known, n.conf.SyncLimit)

	n.coreLock.Unlock()

	if overSyncLimit {
		n.logger.Debug("SyncLimit")

		resp.SyncLimit = true
	} else {
		//Compute Diff
		start := time.Now()
		n.coreLock.Lock()
		eventDiff, err := n.core.EventDiff(cmd.Known)
		n.coreLock.Unlock()
		elapsed := time.Since(start)

		n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Diff()")

		if err != nil {
			n.logger.WithField("error", err).Error("Calculating Diff")
			respErr = err
		}

		//Convert to WireEvents
		wireEvents, err := n.core.ToWire(eventDiff)
		if err != nil {
			n.logger.WithField("error", err).Debug("Converting to WireEvent")
			respErr = err
		} else {
			resp.Events = wireEvents
		}
	}

	//Get Self Known
	n.coreLock.Lock()
	knownEvents := n.core.KnownEvents()
	n.coreLock.Unlock()

	resp.Known = knownEvents

	n.logger.WithFields(logrus.Fields{
		"events":     len(resp.Events),
		"known":      resp.Known,
		"sync_limit": resp.SyncLimit,
		"error":      respErr,
	}).Debug("Responding to SyncRequest")

	rpc.Respond(resp, respErr)
}

func (n *Node) processJoinRequest(rpc net.RPC, cmd *net.JoinRequest) {
	n.logger.WithFields(logrus.Fields{
		"from_id": cmd.FromID,
		"peer":    cmd.Peer,
	}).Debug("process JoinRequest")

	resp := &net.JoinResponse{
		FromID: n.id,
		Peer:   *n.core.peers.ByID[n.id],
	}

	var respErr error

	// n.coreLock.Lock()

	//XXX: pass through the proxy to validate the new peer before adding it
	n.addInternalTransaction(hg.NewInternalTransactionJoin(cmd.Peer))

	if n.core.peers.Len() == 1 {
		//force consensus
		for i := 0; i < 10; i++ {
			if err := n.core.AddSelfEvent(""); err != nil {
				respErr = err

				break
			}

		}

	}

	if err := n.core.RunConsensus(); err != nil {
		respErr = err

		// break
	}

	if n.core.hg.AnchorBlock == nil {
		anchor := n.core.hg.Store.LastBlockIndex()
		n.core.hg.AnchorBlock = &anchor
	}

	n.logger.Error("ANCHOR ", n.core.hg.Store.LastBlockIndex(), n.core.hg.Store.LastRound())

	// if respErr != nil {
	// 	rpc.Respond(&net.FastForwardResponse{}, respErr)

	// 	return
	// }

	// n.coreLock.Unlock()

	// n.processFastForwardRequest(rpc, &net.FastForwardRequest{cmd.FromID})

	rpc.Respond(resp, respErr)
}

func (n *Node) processEagerSyncRequest(rpc net.RPC, cmd *net.EagerSyncRequest) {
	n.logger.WithFields(logrus.Fields{
		"from_id": cmd.FromID,
		"events":  len(cmd.Events),
	}).Debug("EagerSyncRequest")

	success := true

	n.coreLock.Lock()
	err := n.sync(cmd.Events)
	n.coreLock.Unlock()

	if err != nil {
		n.logger.WithField("error", err).Error("sync()")
		success = false
	}

	resp := &net.EagerSyncResponse{
		FromID:  n.id,
		Success: success,
	}

	rpc.Respond(resp, err)
}

func (n *Node) processFastForwardRequest(rpc net.RPC, cmd *net.FastForwardRequest) {
	n.logger.WithFields(logrus.Fields{
		"from": cmd.FromID,
	}).Debug("process FastForwardRequest")

	resp := &net.FastForwardResponse{
		FromID: n.id,
	}

	var respErr error

	//Get latest Frame
	n.coreLock.Lock()

	block, frame, err := n.core.GetAnchorBlockWithFrame()

	n.coreLock.Unlock()

	if err != nil {
		n.logger.WithField("error", err).Error("Getting Frame")

		respErr = err
	}

	resp.Block = *block

	resp.Frame = *frame

	//Get snapshot
	snapshot, err := n.proxy.GetSnapshot(block.Index())

	if err != nil {
		n.logger.WithField("error", err).Error("Getting Snapshot")

		respErr = err
	}

	resp.Snapshot = snapshot

	n.logger.WithFields(logrus.Fields{
		"Events": len(resp.Frame.Events),
		"Error":  respErr,
	}).Debug("Responding to FastForwardRequest")

	rpc.Respond(resp, respErr)
}

//This function is usually called in a go-routine and needs to inform the
//calling routine (usually the babble routine) when it is time to exit the
//Babbling state and return.
func (n *Node) gossip(peer *peers.Peer, parentReturnCh chan struct{}) error {
	//pull
	syncLimit, otherKnownEvents, err := n.pull(peer)

	if err != nil {
		// n.addInternalTransaction(hg.NewInternalTransactionLeave(*peer))

		return err
	}

	//check and handle syncLimit
	if syncLimit {
		n.logger.WithField("from", peer.ID).Debug("SyncLimit")
		n.setState(CatchingUp) //
		parentReturnCh <- struct{}{}

		return nil
	}

	//push
	err = n.push(peer, otherKnownEvents)

	if err != nil {
		return err
	}

	//update peer selector
	n.core.selectorLock.Lock()

	n.core.peerSelector.UpdateLast(peer.ID)

	n.core.selectorLock.Unlock()

	n.logStats()

	return nil
}

func (n *Node) pull(peer *peers.Peer) (syncLimit bool, otherKnownEvents map[uint32]int, err error) {
	//Compute Known
	n.coreLock.Lock()

	knownEvents := n.core.KnownEvents()

	n.coreLock.Unlock()

	//Send SyncRequest
	start := time.Now()

	resp, err := n.requestSync(peer.NetAddr, knownEvents)

	elapsed := time.Since(start)

	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("requestSync()")

	if err != nil {
		n.logger.WithField("error", err).Error("requestSync()")

		return false, nil, err
	}

	n.logger.WithFields(logrus.Fields{
		"from_id":    resp.FromID,
		"sync_limit": resp.SyncLimit,
		"events":     len(resp.Events),
		"known":      resp.Known,
	}).Debug("SyncResponse")

	if resp.SyncLimit {
		return true, nil, nil
	}

	//Add Events to Hashgraph and create new Head if necessary
	n.coreLock.Lock()
	err = n.sync(resp.Events)
	n.coreLock.Unlock()
	if err != nil {
		n.logger.WithField("error", err).Error("sync()")
		return false, nil, err
	}

	return false, resp.Known, nil
}

func (n *Node) push(peer *peers.Peer, knownEvents map[uint32]int) error {

	//Check SyncLimit
	n.coreLock.Lock()

	overSyncLimit := n.core.OverSyncLimit(knownEvents, n.conf.SyncLimit)

	n.coreLock.Unlock()

	if overSyncLimit {
		n.logger.Debug("SyncLimit")

		return nil
	}

	//Compute Diff
	start := time.Now()

	n.coreLock.Lock()

	eventDiff, err := n.core.EventDiff(knownEvents)

	n.coreLock.Unlock()

	elapsed := time.Since(start)

	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Diff()")

	if err != nil {
		n.logger.WithField("error", err).Error("Calculating Diff")

		return err
	}

	if len(eventDiff) > 0 {
		//Convert to WireEvents
		wireEvents, err := n.core.ToWire(eventDiff)
		if err != nil {
			n.logger.WithField("error", err).Debug("Converting to WireEvent")
			return err
		}

		//Create and Send EagerSyncRequest
		start = time.Now()
		resp2, err := n.requestEagerSync(peer.NetAddr, wireEvents)
		elapsed = time.Since(start)
		n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("requestEagerSync()")
		if err != nil {
			n.logger.WithField("error", err).Error("requestEagerSync()")
			return err
		}
		n.logger.WithFields(logrus.Fields{
			"from_id": resp2.FromID,
			"success": resp2.Success,
		}).Debug("EagerSyncResponse")
	}

	return nil
}

func (n *Node) fastForward() error {
	n.logger.Debug("IN CATCHING-UP STATE")

	//wait until sync routines finish
	n.waitRoutines()

	//fastForwardRequest
	peer := n.core.peerSelector.Next()

	// if peer == nil && len(addr) > 0 {
	// 	peer = peers.NewPeer("", addr)
	// }

	start := time.Now()

	resp, err := n.requestFastForward(peer.NetAddr)

	elapsed := time.Since(start)

	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("requestFastForward()")

	if err != nil {
		n.logger.WithField("error", err).Error("requestFastForward()")

		return err
	}

	n.logger.WithFields(logrus.Fields{
		"from_id":              resp.FromID,
		"block_index":          resp.Block.Index(),
		"block_round_received": resp.Block.RoundReceived(),
		"frame_events":         len(resp.Frame.Events),
		"frame_roots":          resp.Frame.Roots,
		"snapshot":             resp.Snapshot,
	}).Debug("FastForwardResponse")

	//prepare core. ie: fresh hashgraph
	n.coreLock.Lock()

	err = n.core.FastForward(peer.PubKeyHex, &resp.Block, &resp.Frame)

	n.coreLock.Unlock()

	if err != nil {
		n.logger.WithField("error", err).Error("Fast Forwarding Hashgraph")
		n.logger.Panic("LOL ", resp.Frame.Round, resp.Block.Index(), len(resp.Frame.Peers))

		return err
	}

	//update app from snapshot
	err = n.proxy.Restore(resp.Snapshot)

	if err != nil {
		n.logger.WithField("error", err).Error("Restoring App from Snapshot")

		return err
	}

	n.logger.Debug("Fast-Forward OK")

	n.setState(Babbling)

	return nil
}

func (n *Node) requestSync(target string, known map[uint32]int) (net.SyncResponse, error) {
	args := net.SyncRequest{
		FromID: n.id,
		Known:  known,
	}

	var out net.SyncResponse

	err := n.trans.Sync(target, &args, &out)

	return out, err
}

func (n *Node) requestJoin(target string) (net.JoinResponse, error) {
	args := net.JoinRequest{
		FromID: n.id,
		Peer:   *n.core.peers.ByID[n.id],
	}

	var out net.JoinResponse

	err := n.trans.Join(target, &args, &out)

	return out, err
}

func (n *Node) requestEagerSync(target string, events []hg.WireEvent) (net.EagerSyncResponse, error) {
	args := net.EagerSyncRequest{
		FromID: n.id,
		Events: events,
	}

	var out net.EagerSyncResponse

	err := n.trans.EagerSync(target, &args, &out)

	return out, err
}

func (n *Node) requestFastForward(target string) (net.FastForwardResponse, error) {
	n.logger.WithFields(logrus.Fields{
		"target": target,
	}).Debug("RequestFastForward()")

	args := net.FastForwardRequest{
		FromID: n.id,
	}

	var out net.FastForwardResponse

	err := n.trans.FastForward(target, &args, &out)

	return out, err
}

func (n *Node) sync(events []hg.WireEvent) error {
	//Insert Events in Hashgraph and create new Head if necessary
	start := time.Now()

	err := n.core.Sync(events)

	elapsed := time.Since(start)

	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Processed Sync()")

	if err != nil {
		return err
	}

	//Run consensus methods
	start = time.Now()

	err = n.core.RunConsensus()

	elapsed = time.Since(start)

	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Processed RunConsensus()")

	if err != nil {
		return err
	}

	return nil
}

func (n *Node) addTransaction(tx []byte) {
	n.coreLock.Lock()

	defer n.coreLock.Unlock()

	n.core.AddTransactions([][]byte{tx})
}

func (n *Node) addInternalTransaction(tx hg.InternalTransaction) {
	n.coreLock.Lock()
	defer n.coreLock.Unlock()

	n.core.AddInternalTransactions([]hg.InternalTransaction{tx})
}

func (n *Node) Shutdown() {
	if n.getState() != Shutdown {
		n.logger.Debug("Shutdown")

		//Exit any non-shutdown state immediately
		n.setState(Shutdown)

		//Stop and wait for concurrent operations
		close(n.shutdownCh)

		n.waitRoutines()

		//For some reason this needs to be called after closing the shutdownCh
		//Not entirely sure why...
		n.controlTimer.Shutdown()

		//transport and store should only be closed once all concurrent operations
		//are finished otherwise they will panic trying to use close objects
		n.trans.Close()

		n.core.hg.Store.Close()
	}
}

func (n *Node) GetStats() map[string]string {
	toString := func(i *int) string {
		if i == nil {
			return "nil"
		}

		return strconv.Itoa(*i)
	}

	timeElapsed := time.Since(n.start)

	consensusEvents := n.core.GetConsensusEventsCount()

	consensusEventsPerSecond := float64(consensusEvents) / timeElapsed.Seconds()

	lastConsensusRound := n.core.GetLastConsensusRoundIndex()

	var consensusRoundsPerSecond float64

	if lastConsensusRound != nil {
		consensusRoundsPerSecond = float64(*lastConsensusRound) / timeElapsed.Seconds()
	}

	s := map[string]string{
		"last_consensus_round":   toString(lastConsensusRound),
		"last_block_index":       strconv.Itoa(n.core.GetLastBlockIndex()),
		"consensus_events":       strconv.Itoa(consensusEvents),
		"consensus_transactions": strconv.Itoa(n.core.GetConsensusTransactionsCount()),
		"undetermined_events":    strconv.Itoa(len(n.core.GetUndeterminedEvents())),
		"transaction_pool":       strconv.Itoa(len(n.core.transactionPool)),
		"num_peers":              strconv.Itoa(n.core.peerSelector.Peers().Len()),
		"sync_rate":              strconv.FormatFloat(n.SyncRate(), 'f', 2, 64),
		"events_per_second":      strconv.FormatFloat(consensusEventsPerSecond, 'f', 2, 64),
		"rounds_per_second":      strconv.FormatFloat(consensusRoundsPerSecond, 'f', 2, 64),
		"round_events":           strconv.Itoa(n.core.GetLastCommitedRoundEventsCount()),
		"id":                     fmt.Sprint(n.id),
		"state":                  n.getState().String(),
	}
	return s
}

func (n *Node) logStats() {
	stats := n.GetStats()

	n.logger.WithFields(logrus.Fields{
		"last_consensus_round":   stats["last_consensus_round"],
		"last_block_index":       stats["last_block_index"],
		"consensus_events":       stats["consensus_events"],
		"consensus_transactions": stats["consensus_transactions"],
		"undetermined_events":    stats["undetermined_events"],
		"transaction_pool":       stats["transaction_pool"],
		"num_peers":              stats["num_peers"],
		"sync_rate":              stats["sync_rate"],
		"events/s":               stats["events_per_second"],
		"rounds/s":               stats["rounds_per_second"],
		"round_events":           stats["round_events"],
		"id":                     stats["id"],
		"state":                  stats["state"],
	}).Debug("Stats")
}

func (n *Node) SyncRate() float64 {
	var syncErrorRate float64

	if n.syncRequests != 0 {
		syncErrorRate = float64(n.syncErrors) / float64(n.syncRequests)
	}

	return 1 - syncErrorRate
}

func (n *Node) GetBlock(blockIndex int) (*hg.Block, error) {
	return n.core.hg.Store.GetBlock(blockIndex)
}

func (n *Node) GetEvents() (map[uint32]int, error) {
	res := n.core.KnownEvents()

	return res, nil
}

func (n *Node) ID() uint32 {
	return n.id
}
