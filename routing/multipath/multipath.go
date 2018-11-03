package multipath

import (
	"bytes"
	"fmt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"net"
	"sync"
	"time"
	"math"
)

type RouterID [33]byte

const (
	FindPathMaxDelay = 3
	UpdateWindow     = 3
	ProbeSendCycle   = 3
	ClearCycle 		 = 10
	BufferSize 		 = 1000
)

type MultiPathRouter struct {
	SelfNode RouterID

	Address []net.Addr

	Neighbours map[RouterID]struct{}

	RoutingTable map[RouterID]map[RouterID]uint8

	BestRoutingTable map[RouterID]*BestTableEntry

	LinkChangeBuff chan *LinkChange

	ProbeBuffer chan *MultiPathProbeMsg

	RequestBuffer chan *MultiPathRequestMsg

	ResponseBuffer chan *MultiPathResponseMsg

	RequestPool map[string]chan lnwire.MultiPathResponse

	SendToPeer func(target *btcec.PublicKey, msgs ...lnwire.Message) error

	ConnectToPeer func(addr *lnwire.NetAddress, perm bool) error

	DisconnectPeer func(pubKey *btcec.PublicKey) error

	FindPeerByPubStr func(pubStr string) bool

	DB *channeldb.DB

	SendTimer *time.Ticker

	ClearTimer *time.Ticker

	wg sync.WaitGroup

	rwMu sync.RWMutex

	mu sync.Mutex

	quit chan struct{}
}

type BestTableEntry struct {
	bestHop     RouterID
	minDis      uint8
	updatedTime int64
	updated     bool
	receivedTime int64
}

type LinkChange struct {
	ChangeType  int
	NeighbourID RouterID
}

func (r *MultiPathRouter) Start() {
	r.wg.Add(1)
	defer r.wg.Done()

	for {
		select {
		case linkChange := <-r.LinkChangeBuff:
			go r.handleLinkChange(linkChange)

		case probe := <-r.ProbeBuffer:
			go r.handleProbe(probe)

		case request := <-r.RequestBuffer:
			go r.handleRequest(request)

		case response := <-r.ResponseBuffer:
			go r.handleResponse(response)

		case <-r.ClearTimer.C:
			go r.clearEntry()

		case <-r.SendTimer.C:
			go r.sendProbe()

		case <-r.quit:
			return
		}
	}
}

func (r *MultiPathRouter) Stop() {
	close(r.quit)
	multiPathLog.Infof("multiPath router recieved close request")

	r.wg.Wait()
	multiPathLog.Infof("multiPath router stopped")
}
func (r *MultiPathRouter) sendProbe() {
	for neighbour := range r.Neighbours{
		probe := &lnwire.HULAProbe{
			Destination: r.SelfNode,
			Distance: 0,
			UpperHop: r.SelfNode,
		}
		neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
		if err != nil {
			multiPathLog.Errorf("cannot parse the key :%V", err)
			continue
		}
		err = r.SendToPeer(neighbourKey, probe)
		if err != nil {
			multiPathLog.Errorf("send the probe to neighbour %v failed :%V",
				neighbour, err)
		}
	}
}

func (r *MultiPathRouter) clearEntry ()  {
	r.rwMu.Lock()
	defer r.rwMu.Unlock()
	for dest, entry := range r.BestRoutingTable{
		timeNow := time.Now().Unix()
		if timeNow - entry.receivedTime >= ClearCycle ||
			entry.minDis > math.MaxInt8 {
			delete(r.RoutingTable, dest)
			delete(r.BestRoutingTable, dest)
			multiPathLog.Infof("remove the entry of the dest to :%v", dest)
		}
	}
}

func NewMultiPathRouter(db *channeldb.DB, selfNode [33]byte,
	addr []net.Addr) *MultiPathRouter {
	router := &MultiPathRouter{
		DB:               db,
		SelfNode:         selfNode,
		ProbeBuffer:      make(chan *MultiPathProbeMsg, BufferSize),
		RequestBuffer:    make(chan *MultiPathRequestMsg, BufferSize),
		ResponseBuffer:   make(chan *MultiPathResponseMsg, BufferSize),
		Neighbours:       make(map[RouterID]struct{}),
		RequestPool:      make(map[string]chan lnwire.MultiPathResponse),
		LinkChangeBuff:   make(chan *LinkChange, BufferSize),
		RoutingTable:     make(map[RouterID]map[RouterID]uint8),
		BestRoutingTable: make(map[RouterID]*BestTableEntry),
		Address:          addr,
		mu:               sync.Mutex{},
		rwMu:             sync.RWMutex{},
		SendTimer:        time.NewTicker(ProbeSendCycle * time.Second),
		ClearTimer:       time.NewTicker(ClearCycle * time.Second),
		wg:               sync.WaitGroup{},
		quit:             make(chan struct{}),
	}
	return router
}

func (r *MultiPathRouter) handleProbe(msg *MultiPathProbeMsg) {
	// 如果probe的目的地是当前节点，所以
	p := msg.msg
	if bytes.Equal(p.Destination[:], r.SelfNode[:]) {
		multiPathLog.Infof("recieved a probe generated from self: %v", p)
		return
	}

	dest := p.Destination
	mapNextHop, ok := r.RoutingTable[dest]
	// 如果在路由表中没有找到， 我们需要在routing table 和 best table中创建关于到dest的表项
	if !ok {
		r.RoutingTable[dest] = make(map[RouterID]uint8)
		r.RoutingTable[dest][p.UpperHop] = p.Distance + 1

		r.BestRoutingTable[dest] = &BestTableEntry{
			updated:     false,
			updatedTime: time.Now().Unix(),
			bestHop:     p.UpperHop,
			minDis:      p.Distance + 1,
			receivedTime:time.Now().Unix(),
		}

		for neighbour := range r.Neighbours {
			probe := &lnwire.MultiPathProbe{
				Destination: dest,
				UpperHop:    r.SelfNode,
				Distance:    p.Distance + 1,
			}

			neighKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
			if err != nil {
				multiPathLog.Errorf("cann't parse the neighbour's key: %v",
					neighbour)
			}

			err = r.SendToPeer(neighKey, probe)
			if err != nil {
				multiPathLog.Errorf("cann't send the probe:%v to neighbour: %v",
					probe, neighKey)
			}
		}
	} else {
		mapNextHop[p.UpperHop] = p.Distance + 1
		r.BestRoutingTable[p.Destination].receivedTime = time.Now().Unix()
		// 如果是最优节点发来的probe
		if bytes.Equal(p.UpperHop[:], r.BestRoutingTable[dest].bestHop[:]) {
			// 比当前最优距离还小，那么肯定是最优解
			if p.Distance+1 < r.BestRoutingTable[dest].minDis {
				r.BestRoutingTable[dest].updated = true
				r.BestRoutingTable[dest].bestHop = p.UpperHop
				r.BestRoutingTable[dest].minDis = p.Distance + 1
			} else {
				// 否则遍历，找到关于dest距离最小的表项，更新bestTable

				newMinDistance := r.BestRoutingTable[dest].minDis
				newBestHop := r.BestRoutingTable[dest].bestHop
				for upper, distance := range mapNextHop {
					if distance < newMinDistance {
						newMinDistance = distance
						newBestHop = upper
					}
				}
				r.BestRoutingTable[dest].updated = true
				r.BestRoutingTable[dest].bestHop = newBestHop
				r.BestRoutingTable[dest].minDis = newMinDistance
			}
		} else {
			// 非最优节点发来的probe
			newMinDistance := r.BestRoutingTable[dest].minDis
			newBestHop := r.BestRoutingTable[dest].bestHop
			bestChanged := false
			for upper, distance := range mapNextHop {
				if distance < newMinDistance {
					newMinDistance = distance
					newBestHop = upper
					bestChanged = true
				}
			}
			if bestChanged {
				r.BestRoutingTable[dest].updated = true
				r.BestRoutingTable[dest].bestHop = newBestHop
				r.BestRoutingTable[dest].minDis = newMinDistance
			}
		}

		timeDiff := time.Now().Unix() - r.BestRoutingTable[dest].updatedTime
		if timeDiff >= UpdateWindow && r.BestRoutingTable[dest].updated {
			for neighbour := range r.Neighbours {
				probe := &lnwire.MultiPathProbe{
					Destination: dest,
					UpperHop:    r.SelfNode,
					Distance:    r.BestRoutingTable[dest].minDis,
				}

				neighKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
				if err != nil {
					multiPathLog.Errorf("cann't parse the neighbour key:%v", neighbour)
				}
				err = r.SendToPeer(neighKey, probe)
				if err != nil {
					multiPathLog.Errorf("send probe: %v to neighbour %v failed",
						probe, neighbour)
				}
				r.BestRoutingTable[dest].updated = false
				r.BestRoutingTable[dest].updatedTime = time.Now().Unix()
			}
		}
	}
}

func (r *MultiPathRouter) handleRequest(req *MultiPathRequestMsg) {
	msg := req.msg
	dest := msg.DestNodeID

	if bytes.Equal(dest[:], r.SelfNode[:]) {
		multiPathLog.Infof("get destination, begin send response")
		multiPathResponse := &lnwire.MultiPathResponse{
			Success:      1,
			PathNodes:    msg.PathNodes,
			PathChannels: msg.PathChannels,
			RequestID:    msg.RequestID,
		}
		multiPathResponse.PathNodes = append(multiPathResponse.PathNodes, r.SelfNode)

		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			return
		}
		find := false
		var candiChannel *channeldb.OpenChannel
		for _, dbChan := range dbChans {
			linkNodeID := dbChan.IdentityPub.SerializeCompressed()
			if bytes.Equal(linkNodeID[:], req.addr.IdentityKey.SerializeCompressed()) {
				if candiChannel == nil {
					candiChannel = dbChan
				} else if candiChannel.LocalCommitment.RemoteBalance <
					dbChan.LocalCommitment.RemoteBalance {
					candiChannel = dbChan
				}
				find = true
			}
		}
		if candiChannel != nil {
			multiPathResponse.PathChannels = append(multiPathResponse.PathChannels,
				candiChannel.FundingOutpoint)
		}
		if find == false {
			multiPathResponse.Success = 0
		}
		if len(msg.Addresses) == 0 {
			multiPathLog.Errorf("we don't know the source node ip of req :%v", msg)
			return
		}
		err = r.SendToPeer(req.addr.IdentityKey, multiPathResponse)
		multiPathLog.Infof("发送response：%v 到： %v", multiPathResponse, req.addr.IdentityKey)
		r.mu.Unlock()
		return

	} else if entry, ok := r.BestRoutingTable[dest]; ok {

		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			return
		}

		var candiChannel *channeldb.OpenChannel
		for _, dbChan := range dbChans {
			linkNodeID := dbChan.IdentityPub.SerializeCompressed()
			if bytes.Equal(linkNodeID[:], req.addr.IdentityKey.SerializeCompressed()) {
				if candiChannel == nil {
					candiChannel = dbChan
				} else if candiChannel.LocalCommitment.RemoteBalance <
					dbChan.LocalCommitment.RemoteBalance {
					candiChannel = dbChan
				}
			}
		}
		if candiChannel != nil {
			msg.PathChannels = append(msg.PathChannels,
				candiChannel.FundingOutpoint)
			msg.PathNodes = append(msg.PathNodes, r.SelfNode)
		}
		peerPubKey, err := btcec.ParsePubKey(entry.bestHop[:], btcec.S256())
		if err != nil {
			multiPathLog.Errorf("cann't parse the key :%v", entry.bestHop)
			return
		}
		err = r.SendToPeer(peerPubKey, msg)
		multiPathLog.Infof("send the multiPath request to nextHop :%v", peerPubKey)
		return

	} else {
		multiPathLog.Infof("we can't find the entry to arrive the dest\n")
		multiPathResponse := &lnwire.MultiPathResponse{
			RequestID: msg.RequestID,
			Success:   0,
		}
		err := r.SendToPeer(req.addr.IdentityKey, multiPathResponse)
		if err != nil {
			multiPathLog.Errorf("send the failure response failed")
		}
		multiPathLog.Infof("sent the failure response to :%v", req.addr.IdentityKey)
		return
	}
}

// TODO(xuehan): add the support for multi-path
func (r *MultiPathRouter) handleResponse(msg *MultiPathResponseMsg) {
	res := msg.msg
	// 说明response已经回到了发起节点
	if bytes.Equal(res.PathNodes[0][:], r.SelfNode[:]) {
		if res.Success == 1 {
			r.RequestPool[string(res.RequestID[:])] <- *res
			multiPathLog.Infof("successfully recieved a payment response")
			return
		} else {
			// TODO(xuehan): show the reason and the other detail.
			multiPathLog.Infof("recieved a failed payment response")
			return
		}
	} else {
		for i, node := range res.PathNodes {
			if bytes.Equal(node[:], r.SelfNode[:]) {
				nodeKey, err := btcec.ParsePubKey(res.PathNodes[i-1][:], btcec.S256())
				if err != nil {
					multiPathLog.Errorf("cann't parse the key:%v", node)
					return
				}
				err = r.SendToPeer(nodeKey, res)
				if err != nil {
					multiPathLog.Errorf("send the response to next node failed:%v", err)
					return
				}
			}
		}
	}
}

func (r *MultiPathRouter) handleLinkChange(change *LinkChange) {
	// if this is an add type, we solve the change.
	multiPathLog.Infof("hula router recieve linkchange %v", change)
	if change.ChangeType == 1 {
		r.rwMu.Lock()
		r.Neighbours[change.NeighbourID] = struct{}{}
		r.rwMu.Unlock()

	} else if change.ChangeType == 0 {
		// if this is remove type, check if remove the neighbour.
		find := false
		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			multiPathLog.Errorf("fetch open channels failed:%v", err)
		}
		for _, dbChan := range dbChans {
			if bytes.Equal(dbChan.IdentityPub.SerializeCompressed(),
				change.NeighbourID[:]) {
				find = true
			}
		}
		if find == false {
			r.rwMu.Lock()
			delete(r.Neighbours, change.NeighbourID)
			delete(r.BestRoutingTable, change.NeighbourID)
			delete(r.RoutingTable, change.NeighbourID)
			r.rwMu.Unlock()

			for neighbour := range r.Neighbours{
				probe := &lnwire.MultiPathProbe{
					Destination: change.NeighbourID,
					Distance: math.MaxUint8,
					UpperHop: r.SelfNode,
					Capacity: 0,
				}
				neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
				if err != nil {
					multiPathLog.Errorf("cannot parse the key :%V", err)
					continue
				}
				err = r.SendToPeer(neighbourKey, probe)
				if err != nil {
					multiPathLog.Errorf("send the probe to neighbour %v failed :%V",
						neighbour, err)
				}
			}
			multiPathLog.Infof("the neighbour: %v was removed, and send the probe " +
				"to neighbours")
		}
	}
	multiPathLog.Infof("multiPath router solved the linkchange " +
		"neighbours is %v", r.Neighbours)
}

func (r *MultiPathRouter) FindPath(dest [33]byte, amt btcutil.Amount) (
	[]wire.OutPoint, [][33]byte, error) {
	r.rwMu.RLock()
	entry, ok := r.BestRoutingTable[dest]
	r.rwMu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("cann't find the entry in table or amt insufficient")
	}
	multiPathRequest := &lnwire.MultiPathRequest{
		SourceNodeID: r.SelfNode,
		Addresses:    r.Address,
		DestNodeID:   dest,
	}
	requestID := []byte(routing.GenRequestID(string(r.SelfNode[:])))
	copy(multiPathRequest.RequestID[:], requestID)
	multiPathLog.Infof("new hualRequest is :%v", multiPathRequest)

	r.mu.Lock()
	r.RequestPool[string(requestID)] = make(chan lnwire.MultiPathResponse)
	r.mu.Unlock()
	multiPathLog.Infof("添加到requestPool")

	nextNodeKye, err := btcec.ParsePubKey(entry.bestHop[:], btcec.S256())
	if err != nil {
		return nil, nil, err
	}
	multiPathLog.Infof("send the multiPathReqest: %v to nextHop: %v\n",
		multiPathRequest, nextNodeKye)
	err = r.SendToPeer(nextNodeKye, multiPathRequest)
	if err != nil {
		return nil, nil, err
	}

	select {
	case response := <-r.RequestPool[string(requestID)]:
		multiPathLog.Infof("recieved the multiPathResponse: %v ", response)
		delete(r.RequestPool, string(requestID))
		return response.PathChannels, response.PathNodes, nil
	case <-time.After(FindPathMaxDelay * time.Second):
		// if timeout, remove the channel from requestPool.
		r.mu.Lock()
		delete(r.RequestPool, string(requestID))
		r.mu.Unlock()
		multiPathLog.Infof("hula findPath time out, the requestID is :%v \n", requestID)
		return nil, nil, fmt.Errorf("timeout for the routing path\n")
	}
	return nil, nil, nil
}

type MultiPathProbeMsg struct {
	msg  *lnwire.MultiPathProbe
	addr *lnwire.NetAddress
}

type MultiPathRequestMsg struct {
	msg  *lnwire.MultiPathRequest
	addr *lnwire.NetAddress
}

type MultiPathResponseMsg struct {
	msg  *lnwire.MultiPathResponse
	addr *lnwire.NetAddress
}

// processMultiPathUpdateMsg sends a message to the MultiPathRouter allowing it to
// update router table.
func (r *MultiPathRouter) ProcessMultiPathUpdateMsg(msg *lnwire.MultiPathProbe,
	peerAddress *lnwire.NetAddress) {
	multiPathLog.Infof("recieved the multiPath probe :%v from %v", msg, peerAddress)
	select {
	case r.ProbeBuffer <- &MultiPathProbeMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processMultiPathRequestMsg sends a message to the MultiPathRouter allowing it to
// update router table.
func (r *MultiPathRouter) ProcessMultiPathRequestMsg(msg *lnwire.MultiPathRequest,
	peerAddress *lnwire.NetAddress) {
	multiPathLog.Infof("recieved the multiPath request:%v from %v", msg, peerAddress)
	select {
	case r.RequestBuffer <- &MultiPathRequestMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processMultiPathResponseMsg sends a message to the MultiPathRouter allowing it to
// update router table.
func (r *MultiPathRouter) ProcessMultiPathResponseMsg(msg *lnwire.MultiPathResponse,
	peerAddress *lnwire.NetAddress) {

	multiPathLog.Infof("recieved the multiPath response:%v from %v", msg, peerAddress)
	select {
	case r.ResponseBuffer <- &MultiPathResponseMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}
