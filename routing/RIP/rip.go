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
	"github.com/davecgh/go-spew/spew"
)

const NUM_RIP_BUFFER = 10

const (
	LINK_ADD    = 1
	LINK_REMOVE = 2
)

type RIPRouter struct {
	RouteTable   []*ripRouterEntry
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
	Distance uint8
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
		Neighbours :    make(map[[33]byte]struct{}),
        requestPool: make(map[string]chan lnwire.RIPResponse),
		RouteTable:   make([]*ripRouterEntry, 0),
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
				ripLog.Infof("add link")
				entry := &ripRouterEntry{
					Dest: linkChange.NeighbourID,
					NextHop: linkChange.NeighbourID,
					Distance: 1,
				}
				if exEntry, err := r.findEntry(linkChange.NeighbourID[:]); err == nil {
					exEntry.NextHop = linkChange.NeighbourID
					exEntry.Distance = 1
				} else {
					r.RouteTable = append(r.RouteTable, entry)
				}
				r.sendToNeighbours(linkChange.NeighbourID, linkChange.NeighbourID, 1)
				ripLog.Debugf("router table : %v",
					newLogClosure(func() string {
						return spew.Sdump(r.RouteTable)
				}),
				)
				neighbourPubKey, err := btcec.ParsePubKey(linkChange.NeighbourID[:], btcec.S256())
				if err != nil {
					ripLog.Errorf("can not parse the pub key: %v", linkChange.NeighbourID)
				}
				// We send the route table to the new neighbour.
				for _, routeEntry := range r.RouteTable  {
					update := &lnwire.RIPUpdate{
						Distance: routeEntry.Distance,
						Destination: routeEntry.Dest,
					}
					r.SendToPeer(neighbourPubKey, update)
				}

			// If the remote node was down or the channel was closed,
			// We delete it and notify our neighbour.
			case LINK_REMOVE:
				delete(r.Neighbours, linkChange.NeighbourID)
				ripLog.Infof("rm Link")
				entry, err := r.findEntry(linkChange.NeighbourID[:])

				if err != nil {
					ripLog.Infof("%v", err)
					continue
				}
				entry.Distance = 16
				entry.NextHop = linkChange.NeighbourID
				entry.Dest = linkChange.NeighbourID
				r.sendToNeighbours(linkChange.NeighbourID, linkChange.NeighbourID, 16)
				ripLog.Debugf("router table : %v",
					newLogClosure(func() string {
						return spew.Sdump(r.RouteTable)
					}),
				)
			}
		case ripRequest := <- r.RequestBuffer:
			ripLog.Debugf("revieved the ripRequest:%v", ripRequest)
			r.handleRipRequest(ripRequest)

		case ripResponse := <- r.ResponseBuffer:
			ripLog.Infof("responseBUffer recived")

			ripLog.Infof("%v", r.handleRipResponse(ripResponse))

		case <-r.quit:
			//TODO: add log
			return
		}
	}
}

func (r *RIPRouter) Stop() {
	ripLog.Infof( "rip stopped ")
	close(r.quit)
}

func (r *RIPRouter) handleUpdate(update *lnwire.RIPUpdate, source [33]byte) error {
	nextHop := source
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
			ripLog.Infof("route table entry old is: %v \n", entry)

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
			ripLog.Infof("route table entry new is %v: \n", entry)
		}
	}
	// if this entry is not in the route table, we add this update into table
	if !ifExist && !bytes.Equal(dest[:], r.SelfNode[:]){
		newEntry := ripRouterEntry{
			Distance: distance,
			Dest:     dest,
		}
		copy(newEntry.NextHop[:], nextHop[:])
		r.RouteTable = append(r.RouteTable, &newEntry)
		ifUpdate = true
		ripLog.Infof("route table add entry: %v \n", newEntry)
	}
	// BroadCast this update to his neighbours
	if ifUpdate {
		ripLog.Infof("更新过了,发送更新")
		err := r.sendToNeighbours(source, dest, distance)
		if err != nil {
			return err
		}
	}

	ripLog.Debugf("router table : %v",
		newLogClosure(func() string {
			return spew.Sdump(r.RouteTable)
		}),
	)
	return nil
}

func (r *RIPRouter) sendToNeighbours (source [33]byte,
	dest [33]byte, distance uint8 ) error {
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

			ripLog.Debugf("router table : %v",
				newLogClosure(func() string {
					return spew.Sdump(update)
				}),
			)
			r.SendToPeer(peerPubKey, &update)

			ripLog.Infof("send the rip update:%v to %v\n", update, peerPubKey)
		}
	}
	return  nil
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

	ripLog.Infof("new ripRequest is: %v\n", ripRequest)

	r.mu.Lock()
	r.requestPool[string(requestID)] = make(chan lnwire.RIPResponse)
	r.mu.Unlock()
	ripLog.Infof("添加到requestpool")
	entry, err := r.findEntry(dest[:])
	if err != nil {
		return nil, nil, err
	}
	ripLog.Infof("找到entry")
	nextNodeKye, err  := btcec.ParsePubKey(entry.NextHop[:], btcec.S256())
	if err != nil {
		return nil, nil, err
	}
	ripLog.Infof("send the ripReqest: %v to nextHop: %v\n", nextNodeKye)
	err = r.SendToPeer(nextNodeKye, ripRequest)
	if err != nil {
		return nil, nil, err
	}
	select {
	case response := <- r.requestPool[string(requestID)]:
		ripLog.Infof("recieved the ripResponse: %v ", response)
		return response.PathChannels,response.PathNodes, nil
	case <-time.After(5 * time.Second):
		// if timeout, remove the channel from requestPool.
		r.mu.Lock()
		delete(r.requestPool, string(requestID))
		r.mu.Unlock()

		ripLog.Infof("rip findPath time out, the requestID is :%v \n", requestID)

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
		ripLog.Infof( "我们是终点节点，需要发送response")
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
		// some times this value may be null, find the reason.
		if len(ripReuest.Addresses) == 0 {
			return fmt.Errorf("source address of ripRequest is null, cann't send" +
				"response to the source node")
		}
		sourceAddr := ripReuest.Addresses[0]
		identityKey, err := btcec.ParsePubKey(ripReuest.SourceNodeID[:],btcec.S256())
		if err != nil {
			return err
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address: sourceAddr,
		}
		r.mu.Lock()
		connectedToSource := r.FindPeerByPubStr(string(ripReuest.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			ripLog.Infof("链接到sourceNode： %v", sourceNetAddr)
			if err != nil {
				return nil
			}
		}
		err = r.SendToPeer(identityKey, ripResponse)
		ripLog.Infof("发送response：%v 到： %v", ripResponse, identityKey)
		if !connectedToSource {
			ripLog.Infof( "断开链接")
			err = r.DisconnectPeer(identityKey)
		}
		r.mu.Unlock()
		return err

	} else if entry, err = r.findEntry(dest[:]); err == nil  {
	// If we arrived in the inter-node
		ripLog.Infof("we are the inter-node for the questID:%v \n", ripReuest.RequestID)
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
				ripLog.Infof("send the ripRequest to nextHop: %v", peerPubKey)
				return err
			}
		}

		//TODO(xuehan): send a failure response to source .

	} else {
		ripLog.Infof("we can't find the entry to arrive the dest\n")
		ripResponse := &lnwire.RIPResponse{
			RequestID: ripReuest.RequestID,
			Success: 0,
		}
		// TODO: This value sometimes will be null, find the reason.
		if len(ripReuest.Addresses) == 0 {
			return fmt.Errorf("the ripRequest doesn't hold the source node address," +
				"we cann't send the response to source node")
		}
		sourceAddr:= ripReuest.Addresses[0]
		identityKey, err := btcec.ParsePubKey(ripReuest.SourceNodeID[:],btcec.S256())
		if err != nil {
			return err
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address: sourceAddr,
		}
		// TODO(xuehan): try all address.
		r.mu.Lock()
		connectedToSource := r.FindPeerByPubStr(string(ripReuest.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			if err != nil {
				return nil
			}
		}
		err = r.SendToPeer(identityKey, ripResponse)
		ripLog.Infof("sent the error response to :%v", identityKey)
		if !connectedToSource {
			//err = r.DisconnectPeer(identityKey)
		}
		r.mu.Unlock()
		return err
	}

	// TODO(xuehan): check if the channel capacity is more than the
	// amount required.

	return  nil
}

func (r *RIPRouter) handleRipResponse (msg *RIPResponseMsg) error {
	ripResponse := msg.msg
	/*
	if !bytes.Equal(msg.addr.IdentityKey.SerializeCompressed(),
		ripResponse.RequestID[:]) || ripResponse.Success != 1 {
		return fmt.Errorf("rip route failed")
	}
	*/
	r.mu.RLock()
	ripLog.Infof("requestID:%v" ,ripResponse.RequestID[:])
	if _,ok := r.requestPool[string(ripResponse.RequestID[:])]; !ok {
		r.mu.RUnlock()
		return 	fmt.Errorf("this response is timed out or no request " +
			"matches")
	}
	ripLog.Infof("requestPool recieved ")
	r.requestPool[string(ripResponse.RequestID[:])] <- *ripResponse
	r.mu.RUnlock()
	return nil
}

// processRIPUpdateMsg sends a message to the RIPRouter allowing it to
// update router table.
func (r *RIPRouter) ProcessRIPUpdateMsg(msg *lnwire.RIPUpdate,
	peerAddress *lnwire.NetAddress) {
	ripLog.Infof("recieved the rip update :%v from %v", msg , peerAddress)
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
	ripLog.Infof("recieved the rip request:%v from %v", msg , peerAddress)
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

	ripLog.Infof("recieved the rip response:%v from %v", msg , peerAddress)
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
			return entry, nil
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
