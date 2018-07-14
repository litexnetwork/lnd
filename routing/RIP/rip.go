package RIP

import (
	"bytes"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"sync"
	"fmt"
	"net"
	"github.com/roasbeef/btcd/wire"
	"crypto/md5"
	"encoding/hex"
	"time"
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
	ConnectToPeer func(addr *lnwire.NetAddress, perm bool) error
	DisconnectPeer func(pubKey *btcec.PublicKey) error
	FindPeerByPubStr func(pubStr string) bool
	DB           *channeldb.DB
	requestPool	  map[string]chan lnwire.RIPResponse
	mu         sync.RWMutex
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
		requestPool: make(map[string]chan lnwire.RIPResponse),
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

// TODO(xuehan): add amount parameter
func (r *RIPRouter) FindPath (dest [33]byte) ([]wire.OutPoint,
	[][33]byte, error) {
	ripRequest := &lnwire.RIPRequest{
		SourceNodeID: r.SelfNode,
		Addresses: r.Address,
		DestNodeID:dest,
	}
	requestID := []byte(genRequestID())
	copy(ripRequest.RequestID[:], requestID)
	r.mu.Lock()
	r.requestPool[string(requestID)] = make(chan lnwire.RIPResponse)
	r.mu.Unlock()
	entry, err := r.findEntry(dest[:])
	if err != nil {
		return nil, nil, err
	}
	nextNodeKye, err  := btcec.ParsePubKey(entry.NextHop[:], btcec.S256())
	if err != nil {
		return nil, nil, err
	}
	err = r.SendToPeer(nextNodeKye, ripRequest)
	if err != nil {
		return nil, nil, err
	}
	select {
	case response := <- r.requestPool[string(requestID)]:
		return response.PathChannels,response.PathNodes, nil
	case <-time.After(5 * time.Second):
		// if timeout, remove the channel from requestPool.
		r.mu.Lock()
		delete(r.requestPool, string(requestID))
		r.mu.Unlock()
		return nil, nil,fmt.Errorf("timeout for the routing path\n")
	}
	return nil, nil,nil
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

		ripResponse.PathNodes = append(ripResponse.PathNodes, r.SelfNode)
		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			return err
		}
		find := false
		for _, dbChan := range dbChans {
			linkNodeID := dbChan.IdentityPub.SerializeCompressed()
			if bytes.Equal(linkNodeID[:], msg.addr.IdentityKey.SerializeCompressed()) {
				ripResponse.PathChannels = append(ripReuest.PathChannels,
					dbChan.FundingOutpoint)
				find = true
				break
			}
		}
		if find == false {ripResponse.Success = 0}

		// TODO(xuehan): try all possible address.
		sourceAddr, ok := ripReuest.Addresses[0].(*net.Addr)
		if !ok {
			return fmt.Errorf("can not solve the address : %v\n", r.Address[0])
		}
		identityKey, err := btcec.ParsePubKey(ripReuest.SourceNodeID[:],btcec.S256())
		if err != nil {
			return err
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address: sourceAddr,
		}
		connectedToSource := r.FindPeerByPubStr(string(ripReuest.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			if err != nil {
				return nil
			}
		}
		err = r.SendToPeer(identityKey, ripResponse)
		if !connectedToSource {
			err = r.DisconnectPeer(identityKey)
		}
		return err

	} else if entry, err = r.findEntry(dest[:]); err == nil  {
	// If we arrived in the inter-node
		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			return err
		}
		for _, dbChan := range dbChans {
			linkNodeID := dbChan.IdentityPub.SerializeCompressed()
			if bytes.Equal(linkNodeID[:], msg.addr.IdentityKey.SerializeCompressed()) {
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

		//TODO(xuehan): send a failure response to source .

	} else {
		ripResponse := &lnwire.RIPResponse{
			RequestID: ripReuest.RequestID,
			Success: 0,
		}
		sourceAddr, ok := ripReuest.Addresses[0].(*net.Addr)
		if !ok {
			return fmt.Errorf("can not solve the address : %v\n", r.Address[0])
		}
		identityKey, err := btcec.ParsePubKey(ripReuest.SourceNodeID[:],btcec.S256())
		if err != nil {
			return err
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address: sourceAddr,
		}
		// TODO(xuehan): try all address.
		connectedToSource := r.FindPeerByPubStr(string(ripReuest.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			if err != nil {
				return nil
			}
		}
		err = r.SendToPeer(identityKey, ripResponse)
		if !connectedToSource {
			err = r.DisconnectPeer(identityKey)
		}
		return err
	}

	// TODO(xuehan): check if the channel capacity is more than the
	// amount required.

	return  nil
}

func (r *RIPRouter) handleRipResponse (msg *RIPResponseMsg) error {
	ripResponse := msg.msg
	if !bytes.Equal(msg.addr.IdentityKey.SerializeCompressed(),
		ripResponse.RequestID[:]) || ripResponse.Success != 1 {
		return fmt.Errorf("rip route failed")
	}
	r.mu.RLock()
	if _,ok := r.requestPool[string(ripResponse.RequestID[:])]; !ok {
		return 	fmt.Errorf("this response is timed out or no request " +
			"matches")
	}
	r.requestPool[string(ripResponse.RequestID[:])] <- *ripResponse
	r.mu.Unlock()
	return nil
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
func (r *RIPRouter) findEntry (dest []byte) (*ripRouterEntry, error) {
	for _, entry := range r.RouteTable {
		if bytes.Equal(dest, entry.Dest[:]) {
			return &entry, nil
		}
	}
	return nil, fmt.Errorf("RIP router cann't find the destination : %v  in" +
		"route table", dest)
}

func genRequestID() string {
	str := MD5(time.Now().String())
	return "0" + str
}

func MD5(text string) string{
	ctx := md5.New()
	ctx.Write([]byte(text))
	return hex.EncodeToString(ctx.Sum(nil))
}
