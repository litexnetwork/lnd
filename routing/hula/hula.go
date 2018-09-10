package hula

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"net"
	"sync"
	"time"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcutil"
	"bytes"
	"github.com/roasbeef/btcd/wire"
	"fmt"
)

const (
	BufferSize     = 100
	UpdateWindow   = 1
	ProbeSendCyble = 5
)

type RouterID [33]byte

type HulaRouter struct {
	SelfNode RouterID

	Address []net.Addr

	Neighbours map[RouterID]struct{}

	ProbeUpdateTable map[RouterID]int64

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

	timer *time.Ticker

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

// 只能以线程形式启动
func (r *HulaRouter) start() {
	r.wg.Add(1)
	defer r.wg.Done()

	for {
		select {
		case linkChange := <- r.LinkChangeBuff:
			r.handleLinkChange(linkChange)
		case probe := <-r.ProbeBuffer:
			r.handleProbe(probe)
		case request := <-r.RequestBuffer:
			r.handleRequest(request)
		case response := <-r.ResponseBuffer:
			r.handleResponse(response)
		case <-r.quit:
			return
		}
	}
}

func (r *HulaRouter) stop() {
	close(r.quit)
	r.wg.Wait()
}

func (r *HulaRouter) FindPath(dest [33]byte, amt btcutil.Amount) (
	[]wire.OutPoint, [][33]byte, error ) {

	entry, ok := r.BestHopTable[dest]
	if !ok || entry.capacity < amt{
		return nil, nil, fmt.Errorf("cann't find the entry in table or amt insufficient")
	}
	hulaRequest := &lnwire.HULARequest{
		SourceNodeID:r.SelfNode,
		Addresses:r.Address,
		DestNodeID: dest,
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
	case response := <- r.RequestPool[string(requestID)]:
		hulaLog.Infof("recieved the hulaResponse: %v ", response)
		return response.PathChannels, response.PathNodes, nil
	case <-time.After(5 * time.Second):
		// if timeout, remove the channel from requestPool.
		r.mu.Lock()
		delete(r.RequestPool, string(requestID))
		r.mu.Unlock()
		hulaLog.Infof("hula findPath time out, the requestID is :%v \n", requestID)
		return nil, nil, fmt.Errorf("timeout for the routing path\n")
	}
	return nil, nil, nil
}

func (r *HulaRouter) handleLinkChange (change *LinkChange) {
	// if this is an add type, we solve the change.
	if change.ChangeType == 1 {
		r.rwMu.Lock()
		r.Neighbours[change.NeighbourID] = struct{}{}
		r.rwMu.Unlock()

	} else if change.ChangeType == 0 {
	// if this is remove type, solve it.
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
			r.rwMu.Unlock()
		}
	}
}

func (r *HulaRouter) handleProbe(p *HULAProbeMsg) error {
	msg := p.msg
	if routing.IfKeyEqual(msg.Destination, r.SelfNode) {
		return  nil
	}

	bestHopEntry, ok := r.BestHopTable[msg.Destination]
	if !ok {
		r.BestHopTable[msg.Destination] = &HopTableEntry{
			upperHop: msg.UpperHop,
			capacity: msg.Capacity,
			dis: msg.Distance + 1,
		}

		r.ProbeUpdateTable[msg.Destination] = time.Now().Unix()
		for neighbour := range r.Neighbours {
			probe := &lnwire.HULAProbe{
				Destination: msg.Destination,
				UpperHop: r.SelfNode,
				Distance: msg.Distance + 1,
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
		r.BestHopTable[msg.Destination].updated = false

	// 找到关于这个dest的路由表信息，我们根据收到的probe和路由表决定要不要更新路由表
	} else {
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

			lastUpate, ok := r.ProbeUpdateTable[msg.Destination]
			if !ok {
				hulaLog.Errorf("%v do not exist in the update table")
				return nil
			}

			nowTime := time.Now().Unix()
			if nowTime - lastUpate >= UpdateWindow &&
				bestHopEntry.updated == true {
				for neighbour := range r.Neighbours{
					probe := &lnwire.HULAProbe{
						Destination: msg.Destination,
						UpperHop: r.SelfNode,
						Distance: bestHopEntry.dis,
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
				r.ProbeUpdateTable[msg.Destination] = time.Now().Unix()
				bestHopEntry.updated = false
			}
		}
	}
	return nil
}

func (r *HulaRouter) handleRequest(req *HULARequestMsg) {
	msg := req.msg
	dest := msg.DestNodeID

	if bytes.Equal(dest[:], r.SelfNode[:]) {
		hulaLog.Infof("get destination, begin send response")
		hulaResponse := &lnwire.HULAResponse{
			Success: 1,
			PathNodes: msg.PathNodes,
			PathChannels: msg.PathChannels,
			RequestID: msg.RequestID,
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
		identityKey, err := btcec.ParsePubKey(msg.SourceNodeID[:],btcec.S256())
		if err != nil {
			hulaLog.Errorf("cann't parse the key :%v", msg.SourceNodeID)
		}
		sourceNetAddr := &lnwire.NetAddress{
			IdentityKey:identityKey,
			Address:sourceAddr,
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

	} else if entry , ok := r.BestHopTable[dest]; ok {

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
			hulaLog.Errorf("the ripRequest doesn't hold the source node address," +
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
	}
	hulaLog.Infof("requestPool recieved ")
	r.RequestPool[string(msg.RequestID[:])] <- *msg
	r.mu.Unlock()
}

// 定期扫描路由表，如果某entry超过一定时间没有更新过，那么就删除
// 此外如果发现邻居断了，会立即发送断掉的消息给其他邻居以int最大数作为距离
// 在处理最长probe时删掉自己的表中的那一条，并且将该probe发给邻居
// 如果本身就没有那一条，那么说明其他邻居没有依赖它这条路由信息，因此就不用再广播
func newHulaRouter(db *channeldb.DB, selfNode [33]byte,
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
		ProbeUpdateTable: make(map[RouterID]int64),
		Address:          addr,
		mu:               sync.Mutex{},
		rwMu:sync.RWMutex{},
		timer:            time.NewTicker(ProbeSendCyble * time.Second),
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
func (r *HulaRouter)GetMaxBalanceWithPeer (key *btcec.PublicKey) (btcutil.Amount,
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
