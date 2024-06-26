package node

import (
	"crypto/ecdsa"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"strconv"

	hg "github.com/Goplush/lachesisnode/m/hashgraph"
	"github.com/Goplush/lachesisnode/m/net"
	"github.com/Goplush/lachesisnode/m/proxy"
)

type Node struct {
	nodeState

	conf   *Config
	logger *logrus.Entry

	id       int
	core     *Core
	coreLock sync.Mutex

	localAddr string

	peerSelector PeerSelector
	selectorLock sync.Mutex

	trans net.Transport
	netCh <-chan net.RPC

	proxy    proxy.AppProxy
	submitCh chan []byte

	commitCh chan hg.Block

	shutdownCh chan struct{}

	controlTimer *ControlTimer

	start        time.Time
	syncRequests int
	syncErrors   int
}

func NewNode(conf *Config,
	id int,
	key *ecdsa.PrivateKey,
	participants []net.Peer,
	store hg.Store,
	trans net.Transport,
	proxy proxy.AppProxy) *Node {

	localAddr := trans.LocalAddr()

	pmap, _ := store.Participants()

	commitCh := make(chan hg.Block, 400)
	core := NewCore(id, key, pmap, store, commitCh, conf.Logger)

	peerSelector := NewRandomPeerSelector(participants, localAddr)

	node := Node{
		id:           id,
		conf:         conf,
		core:         &core,
		localAddr:    localAddr,
		logger:       conf.Logger.WithField("this_id", id),
		peerSelector: peerSelector,
		trans:        trans,
		netCh:        trans.Consumer(),
		proxy:        proxy,
		submitCh:     proxy.SubmitCh(),
		commitCh:     commitCh,
		shutdownCh:   make(chan struct{}),
		controlTimer: NewRandomControlTimer(conf.HeartbeatTimeout),
		start:        time.Now(),
	}

	//Initialize as Gossiping
	node.setStarting(true)
	node.setState(Gossiping)

	return &node
}

func (n *Node) Init(bootstrap bool) error {
	peerAddresses := []string{}
	for _, p := range n.peerSelector.Peers() {
		peerAddresses = append(peerAddresses, p.NetAddr)
	}
	n.logger.WithField("peers", peerAddresses).Debug("Init Node")

	if bootstrap {
		n.logger.Debug("Bootstrap")
		return n.core.Bootstrap()
	}
	return n.core.Init()
}

func (n *Node) RunAsync(gossip bool) {
	n.logger.Debug("runasync")
	go n.Run(gossip)
}

func (n *Node) Run(gossip bool) {
	//The ControlTimer allows the background routines to control the
	//heartbeat timer when the node is in the Gossiping state. The timer should
	//only be running when there are uncommitted transactions in the system.
	go n.controlTimer.Run()

	//Execute some background work regardless of the state of the node.
	//Process RPC requests as well as SumbitTx and CommitBlock requests
	n.goFunc(n.doBackgroundWork)

	//Execute Node State Machine
	for {
		// Run different routines depending on node state
		state := n.getState()
		n.logger.WithField("state", state.String()).Debug("Run loop")

		switch state {
		case Gossiping:
			n.lachesis(gossip)
		case CatchingUp:
			n.fastForward()
		case Shutdown:
			return
		}
	}
}

func (n *Node) doBackgroundWork() {
	for {
		select {
		case rpc := <-n.netCh:
			n.logger.Debug("Processing RPC")
			n.processRPC(rpc)
			if n.core.NeedGossip() && !n.controlTimer.set {
				n.controlTimer.resetCh <- struct{}{}
			}
		case t := <-n.submitCh:
			n.logger.Debug("Adding Transaction")
			n.addTransaction(t)
			if !n.controlTimer.set {
				n.controlTimer.resetCh <- struct{}{}
			}
		case block := <-n.commitCh:
			n.logger.WithFields(logrus.Fields{
				"index":          block.Index(),
				"round_received": block.RoundReceived(),
				"txs":            len(block.Transactions()),
			}).Debug("Committing Block")
			if err := n.commit(block); err != nil {
				n.logger.WithField("error", err).Error("Committing Block")
			}
		case <-n.shutdownCh:
			return
		}
	}
}

func (n *Node) lachesis(gossip bool) {
	for {
		oldState := n.getState()
		select {
		case <-n.controlTimer.tickCh:
			if gossip {
				proceed, err := n.preGossip()
				if proceed && err == nil {
					n.logger.Debug("Time to gossip!")
					peer := n.peerSelector.Next()
					n.goFunc(func() { n.gossip(peer.NetAddr) })
				}
			}
			if !n.core.NeedGossip() {
				n.controlTimer.stopCh <- struct{}{}
			} else if !n.controlTimer.set {
				n.controlTimer.resetCh <- struct{}{}
			}
		case <-n.shutdownCh:
			return
		}

		newState := n.getState()
		if newState != oldState {
			return
		}
	}
}

func (n *Node) processRPC(rpc net.RPC) {

	if s := n.getState(); s != Gossiping {
		n.logger.WithField("state", s.String()).Debug("Discarding RPC Request")
		//XXX Use a SyncResponse by default but this should be either a special
		//ErrorResponse type or a type that corresponds to the request
		resp := &net.SyncResponse{
			FromID: n.id,
		}
		rpc.Respond(resp, fmt.Errorf("not ready: %s", s.String()))
		return
	}

	switch cmd := rpc.Command.(type) {
	case *net.SyncRequest:
		n.processSyncRequest(rpc, cmd)
	case *net.EagerSyncRequest:
		n.processEagerSyncRequest(rpc, cmd)
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

func (n *Node) preGossip() (bool, error) {
	n.coreLock.Lock()
	defer n.coreLock.Unlock()

	//Check if it is necessary to gossip
	needGossip := n.core.NeedGossip() || n.isStarting()
	if !needGossip {
		n.logger.Debug("Nothing to gossip")
		return false, nil
	}

	//If the transaction pool is not empty, create a new self-event and empty the
	//transaction pool in its payload
	if err := n.core.AddSelfEvent(); err != nil {
		n.logger.WithField("error", err).Error("Adding SelfEvent")
		return false, err
	}

	return true, nil
}

func (n *Node) gossip(peerAddr string) error {
	//pull
	syncLimit, otherKnownEvents, err := n.pull(peerAddr)
	if err != nil {
		return err
	}

	//check and handle syncLimit
	if syncLimit {
		n.logger.WithField("from", peerAddr).Debug("SyncLimit")
		n.setState(CatchingUp)
		return nil
	}

	//push
	err = n.push(peerAddr, otherKnownEvents)
	if err != nil {
		return err
	}

	//update peer selector
	n.selectorLock.Lock()
	n.peerSelector.UpdateLast(peerAddr)
	n.selectorLock.Unlock()

	n.logStats()

	n.setStarting(false)

	return nil
}

func (n *Node) pull(peerAddr string) (syncLimit bool, otherKnownEvents map[int]int, err error) {
	//Compute Known
	n.coreLock.Lock()
	knownEvents := n.core.KnownEvents()
	n.coreLock.Unlock()

	//Send SyncRequest
	start := time.Now()
	resp, err := n.requestSync(peerAddr, knownEvents)
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

func (n *Node) push(peerAddr string, knownEvents map[int]int) error {

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

	//Convert to WireEvents
	wireEvents, err := n.core.ToWire(eventDiff)
	if err != nil {
		n.logger.WithField("error", err).Debug("Converting to WireEvent")
		return err
	}

	//Create and Send EagerSyncRequest
	start = time.Now()
	resp2, err := n.requestEagerSync(peerAddr, wireEvents)
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

	return nil
}

func (n *Node) fastForward() error {
	n.logger.Debug("IN CATCHING-UP STATE")
	n.logger.Debug("fast-sync not implemented yet")

	//XXX Work in Progress on fsync branch

	n.setState(Gossiping)

	return nil
}

func (n *Node) requestSync(target string, known map[int]int) (net.SyncResponse, error) {

	args := net.SyncRequest{
		FromID: n.id,
		Known:  known,
	}

	var out net.SyncResponse
	err := n.trans.Sync(target, &args, &out)

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
	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Processed RunConsensus1()")
	n.logger.WithField("duration", elapsed.Nanoseconds()).Debug("Local Lachesis")
	if err != nil {
		return err
	}

	return nil
}

func (n *Node) commit(block hg.Block) error {

	stateHash, err := n.proxy.CommitBlock(block)
	n.logger.WithFields(logrus.Fields{
		"block":      block.Index(),
		"state_hash": fmt.Sprintf("0x%X", stateHash),
		"err":        err,
	}).Debug("CommitBlock Response")

	block.Body.StateHash = stateHash

	n.coreLock.Lock()
	defer n.coreLock.Unlock()
	sig, err := n.core.SignBlock(block)
	if err != nil {
		return err
	}
	n.core.AddBlockSignature(sig)

	return err
}

func (n *Node) addTransaction(tx []byte) {
	n.coreLock.Lock()
	defer n.coreLock.Unlock()
	n.core.AddTransactions([][]byte{tx})
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
	consensusTransactions := n.core.GetConsensusTransactionsCount()
	transactionsPerSecond := float64(consensusTransactions) / timeElapsed.Seconds()

	lastConsensusRound := n.core.GetLastConsensusRoundIndex()
	var consensusRoundsPerSecond float64
	if lastConsensusRound != nil {
		consensusRoundsPerSecond = float64(*lastConsensusRound) / timeElapsed.Seconds()
	}

	s := map[string]string{
		"last_consensus_round":    toString(lastConsensusRound),
		"time_elapsed":            strconv.FormatFloat(timeElapsed.Seconds(), 'f', 2, 64),
		"heartbeat":               strconv.FormatFloat(n.conf.HeartbeatTimeout.Seconds(), 'f', 2, 64),
		"node_current":            strconv.FormatInt(time.Now().Unix(), 10),
		"node_start":              strconv.FormatInt(n.start.Unix(), 10),
		"last_block_index":        strconv.Itoa(n.core.GetLastBlockIndex()),
		"consensus_events":        strconv.Itoa(consensusEvents),
		"sync_limit":              strconv.Itoa(n.conf.SyncLimit),
		"consensus_transactions":  strconv.Itoa(consensusTransactions),
		"undetermined_events":     strconv.Itoa(len(n.core.GetUndeterminedEvents())),
		"transaction_pool":        strconv.Itoa(len(n.core.transactionPool)),
		"num_peers":               strconv.Itoa(len(n.peerSelector.Peers())),
		"sync_rate":               strconv.FormatFloat(n.SyncRate(), 'f', 2, 64),
		"transactions_per_second": strconv.FormatFloat(transactionsPerSecond, 'f', 2, 64),
		"events_per_second":       strconv.FormatFloat(consensusEventsPerSecond, 'f', 2, 64),
		"rounds_per_second":       strconv.FormatFloat(consensusRoundsPerSecond, 'f', 2, 64),
		"round_events":            strconv.Itoa(n.core.GetLastCommittedRoundEventsCount()),
		"id":                      strconv.Itoa(n.id),
		"state":                   n.getState().String(),
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

func (n *Node) GetParticipants() (map[string]int, error) {
	return n.core.hg.Store.Participants()
}

func (n *Node) GetEvent(event string) (hg.Event, error) {
	return n.core.hg.Store.GetEvent(event)
}

func (n *Node) GetLastEventFrom(participant string) (string, bool, error) {
	return n.core.hg.Store.LastEventFrom(participant)
}

func (n *Node) GetKnownEvents() map[int]int {
	return n.core.hg.Store.KnownEvents()
}

func (n *Node) GetConsensusEvents() []string {
	return n.core.hg.Store.ConsensusEvents()
}

func (n *Node) GetRound(roundIndex int) (hg.RoundInfo, error) {
	return n.core.hg.Store.GetRound(roundIndex)
}

func (n *Node) GetLastRound() int {
	return n.core.hg.Store.LastRound()
}

func (n *Node) GetRoundWitnesses(roundIndex int) []string {
	return n.core.hg.Store.RoundWitnesses(roundIndex)
}

func (n *Node) GetRoundEvents(roundIndex int) int {
	return n.core.hg.Store.RoundEvents(roundIndex)
}

func (n *Node) GetRoot(rootIndex string) (hg.Root, error) {
	return n.core.hg.Store.GetRoot(rootIndex)
}

func (n *Node) GetBlock(blockIndex int) (hg.Block, error) {
	return n.core.hg.Store.GetBlock(blockIndex)
}
