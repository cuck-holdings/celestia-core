package v1

import (
	"errors"
	"fmt"
	"time"

	"github.com/gogo/protobuf/proto"

	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/clist"
	"github.com/tendermint/tendermint/libs/log"
	cmtsync "github.com/tendermint/tendermint/libs/sync"
	"github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/pkg/trace"
	"github.com/tendermint/tendermint/pkg/trace/schema"
	protomem "github.com/tendermint/tendermint/proto/tendermint/mempool"
	"github.com/tendermint/tendermint/types"
)

const (
	MempoolPriorityChannel = byte(0x80)

	mempoolPriorityInterval          = 10 * time.Second
	mempoolPriorityBroadcastMaxBytes = 2 * 1024 * 1024 // 2MB
)

// Reactor handles mempool tx broadcasting amongst peers.
// It maintains a map from peer ID to counter, to prevent gossiping txs to the
// peers you received it from.
type Reactor struct {
	p2p.BaseReactor
	config      *cfg.MempoolConfig
	mempool     *TxMempool
	ids         *mempoolIDs
	traceClient *trace.Client

	sortedTxs                   []*WrappedTx // sorted by priority
	mempoolPriorityIntervalChan chan struct{}
}

type mempoolIDs struct {
	mtx       cmtsync.RWMutex
	peerMap   map[p2p.ID]uint16
	nextID    uint16              // assumes that a node will never have over 65536 active peers
	activeIDs map[uint16]struct{} // used to check if a given peerID key is used, the value doesn't matter
}

// Reserve searches for the next unused ID and assigns it to the
// peer.
func (ids *mempoolIDs) ReserveForPeer(peer p2p.Peer) {
	ids.mtx.Lock()
	defer ids.mtx.Unlock()

	curID := ids.nextPeerID()
	ids.peerMap[peer.ID()] = curID
	ids.activeIDs[curID] = struct{}{}
}

// nextPeerID returns the next unused peer ID to use.
// This assumes that ids's mutex is already locked.
func (ids *mempoolIDs) nextPeerID() uint16 {
	if len(ids.activeIDs) == mempool.MaxActiveIDs {
		panic(fmt.Sprintf("node has maximum %d active IDs and wanted to get one more", mempool.MaxActiveIDs))
	}

	_, idExists := ids.activeIDs[ids.nextID]
	for idExists {
		ids.nextID++
		_, idExists = ids.activeIDs[ids.nextID]
	}
	curID := ids.nextID
	ids.nextID++
	return curID
}

// Reclaim returns the ID reserved for the peer back to unused pool.
func (ids *mempoolIDs) Reclaim(peer p2p.Peer) {
	ids.mtx.Lock()
	defer ids.mtx.Unlock()

	removedID, ok := ids.peerMap[peer.ID()]
	if ok {
		delete(ids.activeIDs, removedID)
		delete(ids.peerMap, peer.ID())
	}
}

// GetForPeer returns an ID reserved for the peer.
func (ids *mempoolIDs) GetForPeer(peer p2p.Peer) uint16 {
	ids.mtx.RLock()
	defer ids.mtx.RUnlock()

	return ids.peerMap[peer.ID()]
}

func newMempoolIDs() *mempoolIDs {
	return &mempoolIDs{
		peerMap:   make(map[p2p.ID]uint16),
		activeIDs: map[uint16]struct{}{0: {}},
		nextID:    1, // reserve unknownPeerID(0) for mempoolReactor.BroadcastTx
	}
}

// NewReactor returns a new Reactor with the given config and mempool.
func NewReactor(config *cfg.MempoolConfig, mempool *TxMempool, traceClient *trace.Client) *Reactor {
	memR := &Reactor{
		config:      config,
		mempool:     mempool,
		ids:         newMempoolIDs(),
		traceClient: traceClient,
	}
	memR.BaseReactor = *p2p.NewBaseReactor("Mempool", memR)
	return memR
}

// InitPeer implements Reactor by creating a state for the peer.
func (memR *Reactor) InitPeer(peer p2p.Peer) p2p.Peer {
	memR.ids.ReserveForPeer(peer)
	return peer
}

// SetLogger sets the Logger on the reactor and the underlying mempool.
func (memR *Reactor) SetLogger(l log.Logger) {
	memR.Logger = l
}

// OnStart implements p2p.BaseReactor.
func (memR *Reactor) OnStart() error {
	if !memR.config.Broadcast {
		memR.Logger.Info("Tx broadcasting is disabled")
	} else {
		go memR.priorityIntervalRoutine()
	}
	return nil
}

// GetChannels implements Reactor by returning the list of channels for this
// reactor.
func (memR *Reactor) GetChannels() []*p2p.ChannelDescriptor {
	largestTx := make([]byte, memR.config.MaxTxBytes)
	batchMsg := protomem.Message{
		Sum: &protomem.Message_Txs{
			Txs: &protomem.Txs{Txs: [][]byte{largestTx}},
		},
	}

	return []*p2p.ChannelDescriptor{
		{
			ID:                  mempool.MempoolChannel,
			Priority:            5,
			RecvMessageCapacity: batchMsg.Size(),
			MessageType:         &protomem.Message{},
		},
		{
			ID:                  MempoolPriorityChannel,
			Priority:            5,
			RecvMessageCapacity: batchMsg.Size(),
			MessageType:         &protomem.Message{},
		},
	}
}

// AddPeer implements Reactor.
// It starts a broadcast routine ensuring all txs are forwarded to the given peer.
func (memR *Reactor) AddPeer(peer p2p.Peer) {
	if memR.config.Broadcast {
		go memR.broadcastTxRoutine(peer)
		go memR.broadcastPriorityTxRoutine(peer)
	}
}

// RemovePeer implements Reactor.
func (memR *Reactor) RemovePeer(peer p2p.Peer, reason interface{}) {
	memR.ids.Reclaim(peer)
	// broadcast routine checks if peer is gone and returns
}

// Receive implements Reactor.
// It adds any received transactions to the mempool.
func (memR *Reactor) ReceiveEnvelope(e p2p.Envelope) {
	memR.Logger.Debug("Receive", "src", e.Src, "chId", e.ChannelID, "msg", e.Message)
	switch msg := e.Message.(type) {
	case *protomem.Txs:
		for _, tx := range msg.Txs {
			schema.WriteMempoolTx(
				memR.traceClient,
				e.Src.ID(),
				tx,
				schema.TransferTypeDownload,
				schema.V1VersionFieldValue,
			)
		}
		protoTxs := msg.GetTxs()
		if len(protoTxs) == 0 {
			memR.Logger.Error("received tmpty txs from peer", "src", e.Src)
			return
		}
		txInfo := mempool.TxInfo{SenderID: memR.ids.GetForPeer(e.Src)}
		if e.Src != nil {
			txInfo.SenderP2PID = e.Src.ID()
		}

		var err error
		for _, tx := range protoTxs {
			ntx := types.Tx(tx)
			err = memR.mempool.CheckTx(ntx, nil, txInfo)
			if errors.Is(err, mempool.ErrTxInCache) {
				memR.Logger.Debug("Tx already exists in cache", "tx", ntx.String())
			} else if err != nil {
				memR.Logger.Info("Could not check tx", "tx", ntx.String(), "err", err)
			}
		}
	default:
		memR.Logger.Error("unknown message type", "src", e.Src, "chId", e.ChannelID, "msg", e.Message)
		memR.Switch.StopPeerForError(e.Src, fmt.Errorf("mempool cannot handle message of type: %T", e.Message))
		return
	}

	// broadcasting happens from go routines per peer
}

func (memR *Reactor) Receive(chID byte, peer p2p.Peer, msgBytes []byte) {
	msg := &protomem.Message{}
	err := proto.Unmarshal(msgBytes, msg)
	if err != nil {
		panic(err)
	}
	uw, err := msg.Unwrap()
	if err != nil {
		panic(err)
	}
	memR.ReceiveEnvelope(p2p.Envelope{
		ChannelID: chID,
		Src:       peer,
		Message:   uw,
	})
}

// PeerState describes the state of a peer.
type PeerState interface {
	GetHeight() int64
}

// Sort txes by priority at a regular interval and signal the broadcast routine.
func (memR *Reactor) priorityIntervalRoutine() {
	memR.mempoolPriorityIntervalChan = make(chan struct{}, 1)
	lastRoutine := time.Now()
	for {
		// Sleep until the next interval.
		select {
		case <-memR.Quit():
			return
		case <-time.After(mempoolPriorityInterval - time.Since(lastRoutine)):
			lastRoutine = time.Now()
		}

		// Sort txes by priority.
		sortedTxs := memR.mempool.allEntriesSorted()

		// Reap enough txes to fill mempoolPriorityBroadcastMaxBytes.
		var totalSize int64
		for i, tx := range sortedTxs {
			totalSize += tx.Size()
			if totalSize > mempoolPriorityBroadcastMaxBytes {
				sortedTxs = sortedTxs[:i]
				break
			}
		}

		memR.sortedTxs = sortedTxs

		// Signal the priority broadcast routine.
		close(memR.mempoolPriorityIntervalChan)
		memR.mempoolPriorityIntervalChan = make(chan struct{}, 1)
	}
}

// Send new high priority mempool txs to peer.
func (memR *Reactor) broadcastPriorityTxRoutine(peer p2p.Peer) {
	peerID := memR.ids.GetForPeer(peer)

	for {
		select {
		case <-memR.mempoolPriorityIntervalChan:
			// We have new high priority txs to broadcast.
		case <-peer.Quit():
			return

		case <-memR.Quit():
			return
		}

		// In case of both memR.mempoolPriorityIntervalChan and peer.Quit() are variable at the same time
		if !memR.IsRunning() || !peer.IsRunning() {
			return
		}

		// Make sure the peer is up to date.
		peerState, ok := peer.Get(types.PeerStateKey).(PeerState)
		if !ok {
			// Peer does not have a state yet. We set it in the consensus reactor, but
			// when we add peer in Switch, the order we call reactors#AddPeer is
			// different every time due to us using a map. Sometimes other reactors
			// will be initialized before the consensus reactor. We should wait a few
			// milliseconds and retry.
			time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// Loop through all the high priority txs.
		for _, memTx := range memR.sortedTxs {
			// Check that tx is still in mempool.
			if !memR.mempool.HasTx(memTx.tx) {
				continue
			}

			// Allow for a lag of 1 block.
			if peerState.GetHeight() < memTx.height-1 {
				time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
				continue
			}

			// NOTE: Transaction batching was disabled due to
			// https://github.com/cometbft/cometbft/issues/5796
			if !memTx.HasPeer(peerID) {
				success := p2p.SendEnvelopeShim(peer, p2p.Envelope{ //nolint: staticcheck
					ChannelID: mempool.MempoolChannel,
					Message:   &protomem.Txs{Txs: [][]byte{memTx.tx}},
				}, memR.Logger)
				if !success {
					time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
					continue
				}
				schema.WriteMempoolTx(
					memR.traceClient,
					peer.ID(),
					memTx.tx,
					schema.TransferTypeUpload,
					schema.V1VersionFieldValue,
				)
			}
		}
	}
}

// Send new mempool txs to peer.
func (memR *Reactor) broadcastTxRoutine(peer p2p.Peer) {
	peerID := memR.ids.GetForPeer(peer)
	var next *clist.CElement

	for {
		// In case of both next.NextWaitChan() and peer.Quit() are variable at the same time
		if !memR.IsRunning() || !peer.IsRunning() {
			return
		}

		// This happens because the CElement we were looking at got garbage
		// collected (removed). That is, .NextWait() returned nil. Go ahead and
		// start from the beginning.
		if next == nil {
			select {
			case <-memR.mempool.TxsWaitChan(): // Wait until a tx is available
				if next = memR.mempool.TxsFront(); next == nil {
					continue
				}

			case <-peer.Quit():
				return

			case <-memR.Quit():
				return
			}
		}

		// Make sure the peer is up to date.
		peerState, ok := peer.Get(types.PeerStateKey).(PeerState)
		if !ok {
			// Peer does not have a state yet. We set it in the consensus reactor, but
			// when we add peer in Switch, the order we call reactors#AddPeer is
			// different every time due to us using a map. Sometimes other reactors
			// will be initialized before the consensus reactor. We should wait a few
			// milliseconds and retry.
			time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// Allow for a lag of 1 block.
		memTx := next.Value.(*WrappedTx)
		if peerState.GetHeight() < memTx.height-1 {
			time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
			continue
		}

		// NOTE: Transaction batching was disabled due to
		// https://github.com/tendermint/tendermint/issues/5796
		if !memTx.HasPeer(peerID) {
			success := p2p.SendEnvelopeShim(peer, p2p.Envelope{ //nolint: staticcheck
				ChannelID: mempool.MempoolChannel,
				Message:   &protomem.Txs{Txs: [][]byte{memTx.tx}},
			}, memR.Logger)
			if !success {
				time.Sleep(mempool.PeerCatchupSleepIntervalMS * time.Millisecond)
				continue
			}
			schema.WriteMempoolTx(
				memR.traceClient,
				peer.ID(),
				memTx.tx,
				schema.TransferTypeUpload,
				schema.V1VersionFieldValue,
			)
		}

		select {
		case <-next.NextWaitChan():
			// see the start of the for loop for nil check
			next = next.Next()

		case <-peer.Quit():
			return

		case <-memR.Quit():
			return
		}
	}
}

//-----------------------------------------------------------------------------
// Messages

// TxsMessage is a Message containing transactions.
type TxsMessage struct {
	Txs []types.Tx
}

// String returns a string representation of the TxsMessage.
func (m *TxsMessage) String() string {
	return fmt.Sprintf("[TxsMessage %v]", m.Txs)
}
