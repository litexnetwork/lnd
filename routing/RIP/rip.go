package RIP

import (
	"bytes"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"sync"
	"fmt"
	"net"
)

const NUM_RIP_BUFFER = 10

const (
	LINK_ADD    = 1
	LINK_REMOVE = 2
)

type RIPRouter struct {
	RouteTable   []ripRouterEntry
	SelfNode     [33]byte
	Address 		[]net.Addr
	UpdateBuffer chan *RIPUpdateMsg
	RequestBuffer chan *RIPRequestMsg
	ResponseBuffer chan *RIPResponseMsg
	LinkChangeChan chan *LinkChange
	Neighbours   map[[33]byte]struct{}
	SendToPeer   func(target *btcec.PublicKey, msgs ...lnwire.Message) error
	DB           *channeldb.DB
	quit         chan struct{}
}

type RIPUpdateMsg struct {
	msg *lnwire.RIPUpdate
	addr *lnwire.NetAddress
}

type RIPRequestMsg struct {
	msg *lnwire.RIPRequest
	addr *lnwire.NetAddress
}

type RIPResponseMsg struct {
	msg *lnwire.RIPResponse
	addr *lnwire.NetAddress
}

type ripRouterEntry struct {
	Dest     [33]byte
	NextHop  [33]byte
	Distance int8
}

type LinkChange struct {
	ChangeType  int
	NeighbourID [33]byte
	//TODO(xuehan):add  balance change
}

func NewRIPRouter(db *channeldb.DB, selfNode [33]byte, addr []net.Addr) *RIPRouter {
	return &RIPRouter{
		DB:           db,
		SelfNode:     selfNode,
		UpdateBuffer: make(chan *RIPUpdateMsg, NUM_RIP_BUFFER),
		RequestBuffer: make(chan *RIPRequestMsg, NUM_RIP_BUFFER),
		ResponseBuffer: make(chan *RIPResponseMsg, NUM_RIP_BUFFER),
		LinkChangeChan: make(chan *LinkChange, NUM_RIP_BUFFER),
		RouteTable:   []ripRouterEntry{},
		Address:      addr,
		quit:         make(chan struct{}),
	}
}

func (r *RIPRouter) Start(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case updateMsg := <-r.UpdateBuffer:
			var sourceKey [33]byte
			copy(sourceKey[:], updateMsg.addr.IdentityKey.SerializeCompressed())
			r.handleUpdate(updateMsg.msg, sourceKey)

		case linkChange := <- r.LinkChangeChan:
			switch linkChange.ChangeType {
			// If there is a new neighbour, we handle it.
			case LINK_ADD:
				r.Neighbours[linkChange.NeighbourID] = struct{}{}
				addUpdate := &lnwire.RIPUpdate{
					Distance: 0,
				}
				copy(addUpdate.Destination[:], linkChange.NeighbourID[:])
				r.handleUpdate(addUpdate, linkChange.NeighbourID)

			// If the remote node was down or the channel was closed,
			// We delete it and notify our neighbour.
			case LINK_REMOVE:
				delete(r.Neighbours, linkChange.NeighbourID)
				rmUpdate := &lnwire.RIPUpdate{
					Distance: 16,
				}
				copy(rmUpdate.Destination[:], linkChange.NeighbourID[:])
				r.handleUpdate(rmUpdate, linkChange.NeighbourID)
			}
		case ripRequest := <- r.RequestBuffer:
			r.handleRipRequest(ripRequest)

		case ripResponse := <- r.ResponseBuffer:
			r.handleRipResponse(ripResponse)

		case <-r.quit:
			//TODO: add log
			return
		}
	}
}

func (r *RIPRouter) Stop() {
	close(r.quit)
}

func (r *RIPRouter) handleUpdate(update *lnwire.RIPUpdate, source [33]byte) error {
	nextHop := r.SelfNode
	distance := update.Distance + 1
	dest := update.Destination

	if distance >= 16 {
		distance = 16
	}

	ifUpdate := false
	ifExist := false
	// Update the router table
	for _, entry := range r.RouteTable {
		if bytes.Equal(entry.Dest[:], dest[:]) {
			ifExist = true
			if bytes.Equal(entry.NextHop[:], nextHop[:]) {
				entry.Distance = distance
				ifUpdate = true
			} else {
				if entry.Distance > distance {
					copy(entry.NextHop[:], dest[:])
					entry.Distance = distance
					ifUpdate = true
				}
			}
		}
	}
	// if this entry is not in the route table, we add this update into table
	if !ifExist {
		newEntry := ripRouterEntry{
			Distance: distance,
			Dest:     dest,
		}
		copy(newEntry.NextHop[:], dest[:])
		r.RouteTable = append(r.RouteTable, newEntry)
	}

	// BroadCast this update to his neighbours
	if ifUpdate {
		for peer, _ := range r.Neighbours {
			if !bytes.Equal(peer[:], source[:]) {
				peerPubKey, err := btcec.ParsePubKey(peer[:], btcec.S256())
				if err != nil {
					//TODO(xuehan): return multi err
					return err
				}
				update := lnwire.RIPUpdate{
					Distance: distance,
				}
				copy(update.Destination[:], dest[:])
				r.SendToPeer(peerPubKey, &update)
			}
		}
	}
	return nil
}

func (r *RIPRouter) handleRipRequest (msg *RIPRequestMsg) error {
	ripReuest := msg.msg
	dest := ripReuest.DestNodeID
	var entry *ripRouterEntry
	var err error
	// If we get arrived in the destination.
	if bytes.Equal(dest[:], r.SelfNode[:]) {
		ripResponse := &lnwire.RIPResponse{
			Success: 1,
		}
		copy(ripResponse.PathChannels, ripReuest.PathChannels)
		copy(ripResponse.PathNodes, ripReuest.PathNodes)
		copy(ripResponse.RequestID[:], ripReuest.RequestID[:])

		sourceAddr := r.Address[0].(*net.Addr)
		// TODO()建立链接
		peerPubKey, err := btcec.ParsePubKey(entry.NextHop[:], btcec.S256())
		if err != nil {
			//TODO(xuehan): return multi err
			return err
		}
		err = r.SendToPeer(peerPubKey, ripReuest)
		// TODO() 断开链接
		return err

		// TODO(xuehan): send this response to the source node.

	} else if entry, err = r.findEntry(&dest); err == nil  {
	// If we arrived in the inter-node
		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			return err
		}
		for _, dbChan := range dbChans {
			linkNodeID := dbChan.IdentityPub.SerializeCompressed()
			if bytes.Equal(linkNodeID[:], entry.NextHop[:]) {
				ripReuest.PathNodes    = append(ripReuest.PathNodes, r.SelfNode)
				ripReuest.PathChannels = append(ripReuest.PathChannels,
					dbChan.FundingOutpoint)
				peerPubKey, err := btcec.ParsePubKey(entry.NextHop[:], btcec.S256())
				if err != nil {
					//TODO(xuehan): return multi err
					return err
				}
				err = r.SendToPeer(peerPubKey, ripReuest)
				return err
			}
		}
	} else {

		return err
	}


	// TODO(xuehan): check if the channel capacity is more than the
	// amount required.

	return  nil
}

func (r *RIPRouter) handleRipResponse (msg *RIPResponseMsg) (
	[]lnwire.ChannelID, error) {

	return nil, nil
}

// processRIPUpdateMsg sends a message to the RIPRouter allowing it to
// update router table.
func (r *RIPRouter) ProcessRIPUpdateMsg(msg *lnwire.RIPUpdate,
	peerAddress *lnwire.NetAddress) {
	select {
	case r.UpdateBuffer <- &RIPUpdateMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processRIPRequestMsg sends a message to the RIPRouter allowing it to
// update router table.
func (r *RIPRouter) ProcessRIPRequestMsg(msg *lnwire.RIPRequest,
	peerAddress *lnwire.NetAddress) {
	select {
	case r.RequestBuffer <- &RIPRequestMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processRIPResponseMsg sends a message to the RIPRouter allowing it to
// update router table.
func (r *RIPRouter) ProcessRIPResponseMsg(msg *lnwire.RIPResponse,
	peerAddress *lnwire.NetAddress) {
	select {
	case r.ResponseBuffer <- &RIPResponseMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// findEntry tries to find the entry, which destination is equal to the
//
func (r *RIPRouter) findEntry (dest *[33]byte) (*ripRouterEntry, error) {
	for _, entry := range r.RouteTable {
		if bytes.Equal(dest[:], entry.Dest[:]) {
			return &entry, nil
		}
	}
	return nil, fmt.Errorf("RIP router cann't find the destination : %v  in" +
		"route table", dest)
}

