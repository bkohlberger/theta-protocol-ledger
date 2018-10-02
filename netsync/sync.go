package netsync

import (
	"bytes"
	"context"
	"encoding/hex"
	"sync"

	"github.com/thetatoken/ukulele/blockchain"
	"github.com/thetatoken/ukulele/common"
	"github.com/thetatoken/ukulele/common/util"
	"github.com/thetatoken/ukulele/core"
	"github.com/thetatoken/ukulele/dispatcher"
	"github.com/thetatoken/ukulele/p2p"
	"github.com/thetatoken/ukulele/rlp"

	p2ptypes "github.com/thetatoken/ukulele/p2p/types"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type MessageConsumer interface {
	AddMessage(interface{})
}

var _ p2p.MessageHandler = (*SyncManager)(nil)

// SyncManager is an intermediate layer between consensus engine and p2p network. Its main responsibilities are to manage
// fast blocks sync among peers and buffer orphaned block/CC. Otherwise messages are passed through to consensus engine.
type SyncManager struct {
	chain      *blockchain.Chain
	consensus  core.ConsensusEngine
	consumer   MessageConsumer
	dispatcher *dispatcher.Dispatcher
	requestMgr *RequestManager

	wg      *sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool

	incoming chan p2ptypes.Message

	logger *log.Entry
}

func NewSyncManager(chain *blockchain.Chain, cons core.ConsensusEngine, network p2p.Network, disp *dispatcher.Dispatcher, consumer MessageConsumer) *SyncManager {
	sm := &SyncManager{
		chain:      chain,
		consensus:  cons,
		consumer:   consumer,
		dispatcher: disp,

		wg:       &sync.WaitGroup{},
		incoming: make(chan p2ptypes.Message, viper.GetInt(common.CfgSyncMessageQueueSize)),
	}
	sm.requestMgr = NewRequestManager(sm)
	network.RegisterMessageHandler(sm)

	logger := util.GetLoggerForModule("sync")
	if viper.GetBool(common.CfgLogPrintSelfID) {
		logger = logger.WithFields(log.Fields{"id": sm.consensus.ID()})
	}
	sm.logger = logger

	return sm
}

func (sm *SyncManager) Start(ctx context.Context) {
	c, cancel := context.WithCancel(ctx)
	sm.ctx = c
	sm.cancel = cancel

	sm.requestMgr.Start(c)

	sm.wg.Add(1)
	go sm.mainLoop()
}

func (sm *SyncManager) Stop() {
	sm.cancel()
}

func (sm *SyncManager) Wait() {
	sm.requestMgr.Wait()
	sm.wg.Wait()
}

func (sm *SyncManager) mainLoop() {
	defer sm.wg.Done()

	for {
		select {
		case <-sm.ctx.Done():
			sm.stopped = true
			return
		case msg := <-sm.incoming:
			sm.processMessage(msg)
		case block := <-sm.requestMgr.C:
			sm.consumer.AddMessage(block)
		}
	}
}

// GetChannelIDs implements the p2p.MessageHandler interface.
func (sm *SyncManager) GetChannelIDs() []common.ChannelIDEnum {
	return []common.ChannelIDEnum{
		common.ChannelIDHeader,
		common.ChannelIDBlock,
		common.ChannelIDProposal,
		common.ChannelIDCC,
		common.ChannelIDVote,
	}
}

// ParseMessage implements p2p.MessageHandler interface.
func (sm *SyncManager) ParseMessage(peerID string, channelID common.ChannelIDEnum,
	rawMessageBytes common.Bytes) (p2ptypes.Message, error) {
	message := p2ptypes.Message{
		PeerID:    peerID,
		ChannelID: channelID,
	}
	data, err := decodeMessage(rawMessageBytes)
	message.Content = data
	return message, err
}

// EncodeMessage implements p2p.MessageHandler interface.
func (sm *SyncManager) EncodeMessage(message interface{}) (common.Bytes, error) {
	return encodeMessage(message)
}

// HandleMessage implements p2p.MessageHandler interface.
func (sm *SyncManager) HandleMessage(msg p2ptypes.Message) (err error) {
	sm.incoming <- msg
	return
}

func (sm *SyncManager) processMessage(message p2ptypes.Message) {
	switch content := message.Content.(type) {
	case dispatcher.InventoryRequest:
		sm.handleInvRequest(message.PeerID, &content)
	case dispatcher.InventoryResponse:
		sm.handleInvResponse(message.PeerID, &content)
	case dispatcher.DataRequest:
		sm.handleDataRequest(message.PeerID, &content)
	case dispatcher.DataResponse:
		sm.handleDataResponse(message.PeerID, &content)
	default:
		sm.logger.WithFields(log.Fields{
			"message": message,
		}).Panic("Received unknown message")
	}
}

func (m *SyncManager) handleInvRequest(peerID string, req *dispatcher.InventoryRequest) {
	m.logger.WithFields(log.Fields{
		"channelID": req.ChannelID,
		"startHash": req.Start,
		"endHash":   req.End,
	}).Debug("Received inventory request")

	switch req.ChannelID {
	case common.ChannelIDBlock:
		blocks := []string{}
		if req.Start == "" {
			m.logger.WithFields(log.Fields{
				"channelID": req.ChannelID,
			}).Error("No start hash is specified in InvRequest")
			return
		}
		curr, err := hex.DecodeString(req.Start)
		if err != nil {
			m.logger.WithFields(log.Fields{
				"channelID": req.ChannelID,
				"start":     req.Start,
			}).Error("Failed to decode start in InvRequest")
			return
		}
		end, err := hex.DecodeString(req.End)
		if err != nil {
			m.logger.WithFields(log.Fields{
				"channelID": req.ChannelID,
				"end":       req.End,
			}).Error("Failed to decode end in InvRequest")
			return
		}
		for i := 0; i < dispatcher.MaxInventorySize; i++ {
			blocks = append(blocks, hex.EncodeToString(curr))
			block, err := m.chain.FindBlock(curr)
			if err != nil {
				m.logger.WithFields(log.Fields{
					"channelID": req.ChannelID,
					"hash":      curr,
				}).Error("Failed to find block with given hash")
				return
			}
			if len(block.Children) == 0 {
				break
			}

			// Fixme: should we only send blocks on the finalized branch?
			curr = block.Children[0]
			if err != nil {
				m.logger.WithFields(log.Fields{
					"err":  err,
					"hash": curr,
				}).Error("Failed to load block")
				return
			}
			if bytes.Compare(curr, end) == 0 {
				blocks = append(blocks, hex.EncodeToString(end))
				break
			}
		}
		resp := dispatcher.InventoryResponse{ChannelID: common.ChannelIDBlock, Entries: blocks}
		m.logger.WithFields(log.Fields{
			"channelID":         resp.ChannelID,
			"len(resp.Entries)": len(resp.Entries),
		}).Debug("Sending inventory response")
		m.dispatcher.SendInventory([]string{peerID}, resp)
	default:
		m.logger.WithFields(log.Fields{"channelID": req.ChannelID}).Error("Unsupported channelID in received InvRequest")
	}

}

func (m *SyncManager) handleInvResponse(peerID string, resp *dispatcher.InventoryResponse) {
	m.logger.WithFields(log.Fields{
		"channelID":   resp.ChannelID,
		"InvResponse": resp,
	}).Debug("Received InvResponse")

	switch resp.ChannelID {
	case common.ChannelIDBlock:
		for _, hashStr := range resp.Entries {
			hash, err := hex.DecodeString(hashStr)
			if err != nil {
				m.logger.WithFields(log.Fields{"channelID": resp.ChannelID, "hashStr": hashStr, "err": err}).Error("Failed to parse hash string in InvResponse")
				return
			}
			m.requestMgr.AddHash(hash, []string{peerID})
		}
	default:
		m.logger.WithFields(log.Fields{
			"channelID": resp.ChannelID,
		}).Error("Unsupported channelID in received InvRequest")
	}
}

func (m *SyncManager) handleDataRequest(peerID string, data *dispatcher.DataRequest) {
	switch data.ChannelID {
	case common.ChannelIDBlock:
		for _, hashStr := range data.Entries {
			hash, err := hex.DecodeString(hashStr)
			if err != nil {
				m.logger.WithFields(log.Fields{
					"channelID": data.ChannelID,
					"hashStr":   hashStr,
					"err":       err,
				}).Error("Failed to parse hash string in DataRequest")
				return
			}
			block, err := m.chain.FindBlock(hash)
			if err != nil {
				m.logger.WithFields(log.Fields{
					"channelID": data.ChannelID,
					"hashStr":   hashStr,
					"err":       err,
				}).Error("Failed to find hash string locally")
				return
			}

			payload, err := rlp.EncodeToBytes(block.Block)
			if err != nil {
				m.logger.WithFields(log.Fields{
					"block": block,
				}).Error("Failed to encode block")
				return
			}
			data := dispatcher.DataResponse{
				ChannelID: common.ChannelIDBlock,
				Payload:   payload,
			}
			m.logger.WithFields(log.Fields{
				"channelID": data.ChannelID,
				"hashStr":   hashStr,
			}).Debug("Sending requested block")
			m.dispatcher.SendData([]string{peerID}, data)
		}
	default:
		m.logger.WithFields(log.Fields{
			"channelID": data.ChannelID,
		}).Error("Unsupported channelID in received DataRequest")
	}
}

func (m *SyncManager) handleDataResponse(peerID string, data *dispatcher.DataResponse) {
	switch data.ChannelID {
	case common.ChannelIDBlock:
		block := core.NewBlock()
		err := rlp.DecodeBytes(data.Payload, block)
		if err != nil {
			m.logger.WithFields(log.Fields{
				"channelID": data.ChannelID,
				"payload":   data.Payload,
				"error":     err,
			}).Error("Failed to decode DataResponse payload")
			return
		}
		m.handleBlock(block)
	case common.ChannelIDVote:
		vote := &core.Vote{}
		err := rlp.DecodeBytes(data.Payload, vote)
		if err != nil {
			m.logger.WithFields(log.Fields{
				"channelID": data.ChannelID,
				"payload":   data.Payload,
				"error":     err,
			}).Error("Failed to decode DataResponse payload")
			return
		}
		m.handleVote(vote)
	case common.ChannelIDProposal:
		proposal := &core.Proposal{}
		err := rlp.DecodeBytes(data.Payload, proposal)
		if err != nil {
			m.logger.WithFields(log.Fields{
				"channelID": data.ChannelID,
				"payload":   data.Payload,
				"error":     err,
			}).Error("Failed to decode DataResponse payload")
			return
		}
		m.handleProposal(proposal)
	default:
		m.logger.WithFields(log.Fields{
			"channelID": data.ChannelID,
		}).Error("Unsupported channelID in received DataResponse")
	}
}

func (sm *SyncManager) handleProposal(p *core.Proposal) {
	if p.CommitCertificate != nil {
		sm.handleCC(p.CommitCertificate)
	}
	sm.handleBlock(p.Block)
}

func (sm *SyncManager) handleBlock(block *core.Block) {
	sm.logger.WithFields(log.Fields{
		"block.Hash":   block.Hash,
		"block.Parent": block.Parent,
	}).Debug("Received block")

	sm.requestMgr.AddBlock(block)
}

func (sm *SyncManager) handleCC(cc *core.CommitCertificate) {
	for _, vote := range cc.Votes.Votes() {
		sm.consumer.AddMessage(&vote)
	}
}

func (sm *SyncManager) handleVote(vote *core.Vote) {
	if vote.Block != nil {
		sm.requestMgr.AddHash(vote.Block.Hash, []string{})
	}
	sm.consumer.AddMessage(vote)
}
