package peer

import (
	"math/rand"
	"sync"

	mm "github.com/thetatoken/theta/common/math"
	nu "github.com/thetatoken/theta/p2p/netutil"
)

const (
	// % of total peers known returned by GetSelection.
	getSelectionPercent = 23

	// min peers that must be returned by GetSelection. Useful for bootstrapping.
	minGetSelection = 32

	// max peers returned by GetSelection
	maxGetSelection = 250
)

//
// PeerTable is a lookup table for peers
//
type PeerTable struct {
	mutex *sync.Mutex

	peerMap map[string]*Peer // map: peerID |-> *Peer
	peers   []*Peer          // For iteration with deterministic order
	addrMap map[*nu.NetAddress]*Peer
}

type PeerIDAddress struct {
	ID   string
	Addr *nu.NetAddress
}

// CreatePeerTable creates an instance of the PeerTable
func CreatePeerTable() PeerTable {
	return PeerTable{
		mutex:   &sync.Mutex{},
		peerMap: make(map[string]*Peer),
		addrMap: make(map[*nu.NetAddress]*Peer),
	}
}

// AddPeer adds the given peer to the PeerTable
func (pt *PeerTable) AddPeer(peer *Peer) bool {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	_, exists := pt.peerMap[peer.ID()]
	if exists {
		// Update existing entry with same ID.
		for i, p := range pt.peers {
			if p.ID() == peer.ID() {
				p.Stop()
				logger.Warnf("Stopping duplicated peer: %v", p.ID())

				if p.IsOutbound() {
					// if an outbound peer is being replaced by an inbound one,
					// preserve the replaced peer's 'isSeed' flag
					peer.SetSeed(p.IsSeed())
				}
				pt.peers[i] = peer
				break
			}
		}
	} else {
		pt.peers = append(pt.peers, peer)
	}

	pt.peerMap[peer.ID()] = peer
	pt.addrMap[peer.NetAddress()] = peer

	return true
}

// DeletePeer deletes the given peer from the PeerTable
func (pt *PeerTable) DeletePeer(peerID string) {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	var peer *Peer
	var ok bool
	if peer, ok = pt.peerMap[peerID]; !ok {
		return
	}

	delete(pt.peerMap, peerID)
	delete(pt.addrMap, peer.NetAddress())
	for idx, peer := range pt.peers {
		if peer.ID() == peerID {
			pt.peers = append(pt.peers[:idx], pt.peers[idx+1:]...)
		}
	}
}

// PurgeOldestPeer purges the oldest peer from the PeerTable
func (pt *PeerTable) PurgeOldestPeer() *Peer {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	var peer *Peer
	for idx, pr := range pt.peers {
		if !pr.IsSeed() {
			peer = pt.peers[idx]
		}
	}
	if peer != nil {
		delete(pt.peerMap, peer.ID())
		pt.peers = pt.peers[1:]
	}
	
	return peer
}

// GetPeer returns the peer for the given peerID (if exists)
func (pt *PeerTable) GetPeer(peerID string) *Peer {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	peer, exists := pt.peerMap[peerID]
	if !exists {
		return nil
	}
	return peer
}

// GetPeerWithAddr returns the peer for the given address (if exists)
func (pt *PeerTable) GetPeerWithAddr(addr *nu.NetAddress) *Peer {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	peer, exists := pt.addrMap[addr]
	if !exists {
		return nil
	}
	return peer
}

// PeerExists indicates whether the PeerTable has a peer for the given peerID
func (pt *PeerTable) PeerExists(peerID string) bool {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	_, exists := pt.peerMap[peerID]
	return exists
}

// PeerAddrExists indicates whether the PeerTable has a peer for the given address
func (pt *PeerTable) PeerAddrExists(addr *nu.NetAddress) bool {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	_, exists := pt.addrMap[addr]
	return exists
}

// GetAllPeers returns all the peers
func (pt *PeerTable) GetAllPeers() *([]*Peer) {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	ret := make([]*Peer, len(pt.peers))
	for i, p := range pt.peers {
		ret[i] = p
	}
	return &ret
}

// GetSelection randomly selects some peers. Suitable for peer-exchange protocols.
func (pt *PeerTable) GetSelection() (peerIDAddrs []PeerIDAddress) {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	if len(pt.peers) == 0 {
		return nil
	}

	peers := make([]*Peer, len(pt.peers))
	copy(peers, pt.peers)

	numPeers := mm.MaxInt(
		mm.MinInt(minGetSelection, len(peers)),
		len(peers)*getSelectionPercent/100)
	numPeers = mm.MinInt(maxGetSelection, numPeers)

	// Fisher-Yates shuffle the array. We only need to do the first
	// `numPeers' since we are throwing the rest.
	for i := 0; i < numPeers; i++ {
		// pick a number between current index and the end
		j := rand.Intn(len(peers)-i) + i
		peers[i], peers[j] = peers[j], peers[i]
	}

	// slice off the limit we are willing to share.
	peers = peers[:numPeers]
	for _, peer := range peers {
		peerIDAddr := PeerIDAddress{
			ID:   peer.ID(),
			Addr: peer.netAddress,
		}
		peerIDAddrs = append(peerIDAddrs, peerIDAddr)
	}
	return
}

// GetTotalNumPeers returns the total number of peers in the PeerTable
func (pt *PeerTable) GetTotalNumPeers() uint {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	return uint(len(pt.peers))
}
