package hula

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

const (
	BufferSize     = 1000
	UpdateWindow   = 3
	ProbeSendCyble = 1
	ClearCycle     = 5
	FindPathMmaxDelay = 5
)

type RouterID [33]byte

type HulaRouter struct {
	SelfNode RouterID

	Address []net.Addr

	Neighbours map[RouterID]struct{}

	ProbeUpdateTable map[RouterID]*UpdateTableEntry

	BestHopTable map[RouterID]*HopTableEntry

	ProbeBuffer chan *HULAProbeMsg

	RequestBuffer chan *HULARequestMsg

	ResponseBuffer chan *HULAResponseMsg

	RequestPool map[string]chan lnwire.HULAResponse

	LinkChangeBuff chan *LinkChange

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

type HopTableEntry struct {
	upperHop RouterID

	dis uint8

	capacity btcutil.Amount

	updated bool
}

type UpdateTableEntry struct {
	updateTime int64

	receiveTime int64
}
// 只能以线程形式启动
func (r *HulaRouter) Start() {
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

func (r *HulaRouter) Stop() {
	close(r.quit)
	hulaLog.Infof("hula recieved close request")

	r.wg.Wait()
	hulaLog.Infof("hula stopped")
}

func (r *HulaRouter) clearEntry () {
	r.rwMu.Lock()
	for key, entry := range r.ProbeUpdateTable {
		if time.Now().Unix() - entry.receiveTime > ClearCycle ||
			r.BestHopTable[key].dis == math.MaxInt8{
			delete(r.ProbeUpdateTable, key)
			delete(r.BestHopTable,key)
			hulaLog.Infof("remove the entry key:%v from updateTable and" +
				"hopTable",key)
		}
	}
	r.rwMu.Unlock()
}

func (r *HulaRouter) sendProbe() {
	for neighbour := range r.Neighbours{
		probe := &lnwire.HULAProbe{
			Destination: r.SelfNode,
			Distance: 0,
			UpperHop: r.SelfNode,
		}
		neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
		if err != nil {
			hulaLog.Errorf("cannot parse the key :%V", err)
			continue
		}
		balance, err := r.GetMaxBalanceWithPeer(neighbourKey)
		if err != nil {
			hulaLog.Errorf("can't find get the balance with peer:%v",
				neighbourKey)
		}
		probe.Capacity = balance
		err = r.SendToPeer(neighbourKey, probe)
		if err != nil {
			hulaLog.Errorf("send the probe to neighbour %v failed :%V",
				neighbour, err)
		}
	}
}

func (r *HulaRouter) FindPath(dest [33]byte, amt btcutil.Amount) (
	[]wire.OutPoint, [][33]byte, error) {
	r.rwMu.RLock()
	entry, ok := r.BestHopTable[dest]
	r.rwMu.RUnlock()
	if !ok || entry.capacity < amt {
		return nil, nil, fmt.Errorf("cann't find the entry in table or amt insufficient")
	}
	hulaRequest := &lnwire.HULARequest{
		SourceNodeID: r.SelfNode,
		Addresses:    r.Address,
		DestNodeID:   dest,
	}
	requestID := []byte(routing.GenRequestID(string(r.SelfNode[:])))
	copy(hulaRequest.RequestID[:], requestID)
	hulaLog.Infof("new hualRequest is :%v", hulaRequest)

	r.mu.Lock()
	r.RequestPool[string(requestID)] = make(chan lnwire.HULAResponse)
	r.mu.Unlock()
	hulaLog.Infof("添加到requestPool")

	nextNodeKye, err := btcec.ParsePubKey(entry.upperHop[:], btcec.S256())
	if err != nil {
		return nil, nil, err
	}
	hulaLog.Infof("send the hulaReqest: %v to nextHop: %v\n", hulaRequest, nextNodeKye)
	err = r.SendToPeer(nextNodeKye, hulaRequest)
	if err != nil {
		return nil, nil, err
	}

	select {
	case response := <-r.RequestPool[string(requestID)]:
		hulaLog.Infof("recieved the hulaResponse: %v ", response)
		delete(r.RequestPool, string(requestID))
		return response.PathChannels, response.PathNodes, nil
	case <-time.After(FindPathMmaxDelay * time.Second):
		// if timeout, remove the channel from requestPool.
		r.mu.Lock()
		delete(r.RequestPool, string(requestID))
		r.mu.Unlock()
		hulaLog.Infof("hula findPath time out, the requestID is :%v \n", requestID)
		return nil, nil, fmt.Errorf("timeout for the routing path\n")
	}
	return nil, nil, nil
}

func (r *HulaRouter) handleLinkChange(change *LinkChange) {
	// if this is an add type, we solve the change.
	hulaLog.Infof("hula router recieve linkchange %v", change)
	if change.ChangeType == 1 {
		r.rwMu.Lock()
		r.Neighbours[change.NeighbourID] = struct{}{}
		r.rwMu.Unlock()

	} else if change.ChangeType == 0 {
		// if this is remove type, check if remove the neighbour.
		find := false
		dbChans, err := r.DB.FetchAllOpenChannels()
		if err != nil {
			hulaLog.Errorf("fetch open channels failed:%v", err)
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
			delete(r.BestHopTable, change.NeighbourID)
			delete(r.ProbeUpdateTable, change.NeighbourID)
			r.rwMu.Unlock()

			for neighbour := range r.Neighbours{
				probe := &lnwire.HULAProbe{
					Destination: change.NeighbourID,
					Distance: math.MaxUint8,
					UpperHop: r.SelfNode,
					Capacity: 0,
				}
				neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
				if err != nil {
					hulaLog.Errorf("cannot parse the key :%V", err)
					continue
				}
				err = r.SendToPeer(neighbourKey, probe)
				if err != nil {
					hulaLog.Errorf("send the probe to neighbour %v failed :%V",
						neighbour, err)
				}
			}
			hulaLog.Infof("the neighbour: %v was removed, and send the probe " +
				"to neighbours")
		}
	}
	hulaLog.Infof("hula router solved the linkchange " +
		"neighbours is %v", r.Neighbours)
}

func (r *HulaRouter) handleProbe(p *HULAProbeMsg) error {
/*
	hulaLog.Debugf("router is :%v",
		newLogClosure(func() string {
			return spew.Sdump(r.BestHopTable)
		}),
	)
*/
	msg := p.msg

	if routing.IfKeyEqual(msg.Destination, r.SelfNode) {
		return nil
	}
	if msg.Distance == math.MaxUint8 {
		msg.Distance = math.MaxUint8 -1
	}
	r.rwMu.RLock()
	bestHopEntry, ok := r.BestHopTable[msg.Destination]
	r.rwMu.RUnlock()
	if !ok {
		r.rwMu.Lock()
		r.BestHopTable[msg.Destination] = &HopTableEntry{
			upperHop: msg.UpperHop,
			capacity: msg.Capacity,
			dis:      msg.Distance + 1,
			updated: false,
		}

		r.ProbeUpdateTable[msg.Destination] = &UpdateTableEntry{
			updateTime: time.Now().Unix(),
			receiveTime: time.Now().Unix(),
		}
		r.rwMu.Unlock()

		for neighbour := range r.Neighbours {
			if bytes.Equal(neighbour[:], p.addr.IdentityKey.SerializeCompressed()[:]) {
				continue
			}
			probe := &lnwire.HULAProbe{
				Destination: msg.Destination,
				UpperHop:    r.SelfNode,
				Distance:    msg.Distance + 1,
			}
			neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
			if err != nil {
				continue
			}
			balance, err := r.GetMaxBalanceWithPeer(neighbourKey)
			if err != nil {
				hulaLog.Errorf("get max balance with peer :%v failed : %v",
					neighbour, err)
				continue
			}
			probe.Capacity = routing.MinAmount(balance, msg.Capacity)
			err = r.SendToPeer(neighbourKey, probe)
			if err != nil {
				hulaLog.Errorf("send probe :%v failed :%v", probe, err)
			}
		}

		// 找到关于这个dest的路由表信息，我们根据收到的probe和路由表决定要不要更新路由表
	} else {
		// 更新关于发送这个probe的收到时间，以标记发送这个probe的source 节点是否还活着
		r.ProbeUpdateTable[msg.Destination].receiveTime = time.Now().Unix()
		r.rwMu.Lock()
		// 如果还是上一跳发来的probe，我们无条件更新
		if bytes.Equal(bestHopEntry.upperHop[:], msg.UpperHop[:]) {
			bestHopEntry.dis = msg.Distance + 1
			bestHopEntry.capacity = msg.Capacity
			bestHopEntry.updated = true

			// 不是上一跳发来的probe， 那么再分析两种情况
		} else {
			// 如果跳数更少， 则更新
			if bestHopEntry.dis > msg.Distance + 1 {
				bestHopEntry.dis = msg.Distance + 1
				copy(bestHopEntry.upperHop[:], msg.UpperHop[:])
				bestHopEntry.capacity = msg.Capacity
				bestHopEntry.updated = true

				// 跳数相同，保留capacity最大的
			} else if bestHopEntry.dis == msg.Distance + 1 &&
				bestHopEntry.capacity < msg.Capacity {
				copy(bestHopEntry.upperHop[:], msg.UpperHop[:])
				bestHopEntry.capacity = msg.Capacity
				bestHopEntry.updated = true
			}
		}
		r.rwMu.Unlock()

		r.rwMu.RLock()
		lastUpate, ok := r.ProbeUpdateTable[msg.Destination]
		r.rwMu.RUnlock()
		if !ok {
			hulaLog.Errorf("%v do not exist in the update table",
				msg.Destination)
			return nil
		}

		nowTime := time.Now().Unix()
		if nowTime-lastUpate.updateTime >= UpdateWindow &&
			bestHopEntry.updated == true {
			for neighbour := range r.Neighbours {
				if bytes.Equal(neighbour[:], p.addr.IdentityKey.SerializeCompressed()[:]) {
					continue
				}
				probe := &lnwire.HULAProbe{
					Destination: msg.Destination,
					UpperHop:    r.SelfNode,
					Distance:    bestHopEntry.dis,
				}
				neighbourKey, err := btcec.ParsePubKey(neighbour[:], btcec.S256())
				if err != nil {
					continue
				}
				balance, err := r.GetMaxBalanceWithPeer(neighbourKey)
				if err != nil {
					hulaLog.Errorf("get max balance with peer :%v failed : %v",
						neighbour, err)
					continue
				}
				probe.Capacity = routing.MinAmount(balance, msg.Capacity)
				err = r.SendToPeer(neighbourKey, probe)
				if err != nil {
					hulaLog.Errorf("send probe :%v failed :%v", probe, err)
				}
			}
			r.rwMu.Lock()
			r.ProbeUpdateTable[msg.Destination].updateTime = time.Now().Unix()
			r.rwMu.Unlock()
			bestHopEntry.updated = false
		}
	}
	/*
	hulaLog.Debugf("router is :%v",
		newLogClosure(func() string {
			return spew.Sdump(r.BestHopTable)
		}),
	)
	*/
	return nil
}

func (r *HulaRouter) handleRequest(req *HULARequestMsg) {
	msg := req.msg
	dest := msg.DestNodeID

	if bytes.Equal(dest[:], r.SelfNode[:]) {
		hulaLog.Infof("get destination, begin send response")
		hulaResponse := &lnwire.HULAResponse{
			Success:      1,
			PathNodes:    msg.PathNodes,
			PathChannels: msg.PathChannels,
			RequestID:    msg.RequestID,
		}
		hulaResponse.PathNodes = append(hulaResponse.PathNodes, r.SelfNode)

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
			hulaResponse.PathChannels = append(hulaResponse.PathChannels,
				candiChannel.FundingOutpoint)
		}
		if find == false {
			hulaResponse.Success = 0
		}

		if len(msg.Addresses) == 0 {
			hulaLog.Errorf("we don't know the source node ip of req :%v", msg)
			return
		}

		sourceAddr := msg.Addresses[0]
		identityKey, err := btcec.ParsePubKey(msg.SourceNodeID[:], btcec.S256())
		if err != nil {
			hulaLog.Errorf("cann't parse the key :%v", msg.SourceNodeID)
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address:     sourceAddr,
		}
		r.mu.Lock()
		connectedToSource := r.FindPeerByPubStr(string(msg.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			hulaLog.Infof("链接到sourceNode： %v", sourceNetAddr)
			if err != nil {
				return
			}
		}
		err = r.SendToPeer(identityKey, hulaResponse)
		hulaLog.Infof("发送response：%v 到： %v", hulaResponse, identityKey)
		if !connectedToSource {
			hulaLog.Infof("断开链接")
			err = r.DisconnectPeer(identityKey)
		}
		r.mu.Unlock()
		return

	} else if entry, ok := r.BestHopTable[dest]; ok {

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
		peerPubKey, err := btcec.ParsePubKey(entry.upperHop[:], btcec.S256())
		if err != nil {
			hulaLog.Errorf("cann't parse the key :%v", entry.upperHop)
			return
		}
		err = r.SendToPeer(peerPubKey, msg)
		hulaLog.Infof("send the hula request to nextHop :%v", peerPubKey)
		return

	} else {
		hulaLog.Infof("we can't find the entry to arrive the dest\n")
		hulaResponse := &lnwire.HULAResponse{
			RequestID: msg.RequestID,
			Success:   0,
		}
		// TODO: This value sometimes will be null, find the reason.
		if len(msg.Addresses) == 0 {
			hulaLog.Errorf("the hulaRequest doesn't hold the source node address," +
				"we cann't send the response to source node")
			return
		}
		sourceAddr := msg.Addresses[0]
		identityKey, err := btcec.ParsePubKey(msg.SourceNodeID[:], btcec.S256())
		if err != nil {
			hulaLog.Errorf("can not parse the key :%v", msg.SourceNodeID[:])
			return
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey: identityKey,
			Address:     sourceAddr,
		}
		// TODO(xuehan): try all address.
		r.mu.Lock()
		connectedToSource := r.FindPeerByPubStr(string(msg.SourceNodeID[:]))
		if !connectedToSource {
			err = r.ConnectToPeer(sourceNetAddr, false)
			if err != nil {
				hulaLog.Errorf(":%v", err)
				return
			}
		}
		err = r.SendToPeer(identityKey, hulaResponse)
		hulaLog.Infof("sent the error response to :%v", identityKey)
		// TODO(xuehan): check here
		if !connectedToSource {
			r.DisconnectPeer(identityKey)
		}
		r.mu.Unlock()
		return
	}
}

func (r *HulaRouter) handleResponse(req *HULAResponseMsg) {
	msg := req.msg
	// TODO(xuehan): check the lock
	r.mu.Lock()
	hulaLog.Infof("requestID:%v", msg.RequestID[:])
	if _, ok := r.RequestPool[string(msg.RequestID[:])]; !ok {
		r.mu.Unlock()
		hulaLog.Errorf("this response is timed out or no request " +
			"matches")
		return
	}
	hulaLog.Infof("requestPool recieved ")
	if msg.Success == 0 {
		hulaLog.Errorf("the result of response returned is failed")
		r.mu.Unlock()
		return
	}
	r.RequestPool[string(msg.RequestID[:])] <- *msg
	r.mu.Unlock()
}

func NewHulaRouter(db *channeldb.DB, selfNode [33]byte,
	addr []net.Addr) *HulaRouter {
	router := &HulaRouter{
		DB:               db,
		SelfNode:         selfNode,
		ProbeBuffer:      make(chan *HULAProbeMsg, BufferSize),
		RequestBuffer:    make(chan *HULARequestMsg, BufferSize),
		ResponseBuffer:   make(chan *HULAResponseMsg, BufferSize),
		Neighbours:       make(map[RouterID]struct{}),
		RequestPool:      make(map[string]chan lnwire.HULAResponse),
		LinkChangeBuff:   make(chan *LinkChange, BufferSize),
		BestHopTable:     make(map[RouterID]*HopTableEntry),
		ProbeUpdateTable: make(map[RouterID]*UpdateTableEntry),
		Address:          addr,
		mu:               sync.Mutex{},
		rwMu:             sync.RWMutex{},
		SendTimer:        time.NewTicker(ProbeSendCyble * time.Second),
		ClearTimer:       time.NewTicker(ClearCycle * time.Second),
		wg:               sync.WaitGroup{},
		quit:             make(chan struct{}),
	}
	return router
}

type LinkChange struct {
	ChangeType  int
	NeighbourID RouterID
}

type HULAProbeMsg struct {
	msg  *lnwire.HULAProbe
	addr *lnwire.NetAddress
}

type HULARequestMsg struct {
	msg  *lnwire.HULARequest
	addr *lnwire.NetAddress
}

type HULAResponseMsg struct {
	msg  *lnwire.HULAResponse
	addr *lnwire.NetAddress
}

// processHulaUpdateMsg sends a message to the HULARouter allowing it to
// update router table.
func (r *HulaRouter) ProcessHULAUpdateMsg(msg *lnwire.HULAProbe,
	peerAddress *lnwire.NetAddress) {
	hulaLog.Infof("recieved the hula probe :%v from %v", msg, peerAddress)
	select {
	case r.ProbeBuffer <- &HULAProbeMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processHULARequestMsg sends a message to the HULARouter allowing it to
// update router table.
func (r *HulaRouter) ProcessHULARequestMsg(msg *lnwire.HULARequest,
	peerAddress *lnwire.NetAddress) {
	hulaLog.Infof("recieved the hula request:%v from %v", msg, peerAddress)
	select {
	case r.RequestBuffer <- &HULARequestMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

// processHULAResponseMsg sends a message to the HULARouter allowing it to
// update router table.
func (r *HulaRouter) ProcessHULAResponseMsg(msg *lnwire.HULAResponse,
	peerAddress *lnwire.NetAddress) {

	hulaLog.Infof("recieved the hula response:%v from %v", msg, peerAddress)
	select {
	case r.ResponseBuffer <- &HULAResponseMsg{msg, peerAddress}:
	case <-r.quit:
		return
	}
}

func (r *HulaRouter) GetMaxBalanceWithPeer(key *btcec.PublicKey) (btcutil.Amount,
	error) {
	dbChans, err := r.DB.FetchAllOpenChannels()
	if err != nil {
		return 0, fmt.Errorf("cann't find the channel with peer :%v", key)
	}
	var candiChannel *channeldb.OpenChannel
	for _, dbChan := range dbChans {
		linkNodeID := dbChan.IdentityPub.SerializeCompressed()
		if bytes.Equal(linkNodeID[:], key.SerializeCompressed()) {
			if candiChannel == nil {
				candiChannel = dbChan
			} else if candiChannel.LocalCommitment.RemoteBalance <
				dbChan.LocalCommitment.RemoteBalance {
				candiChannel = dbChan
			}
		}
	}
	return candiChannel.LocalCommitment.RemoteBalance.ToSatoshis(), nil
}
