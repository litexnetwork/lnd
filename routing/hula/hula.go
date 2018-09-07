package hula

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"net"
	"sync"
	"time"
)

const (
	BufferSize     = 100
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

	RequestPool map[string]chan lnwire.RIPResponse

	LinkChangeBuff chan *LinkChange

	SendToPeer func(target *btcec.PublicKey, msgs ...lnwire.Message) error

	ConnectToPeer func(addr *lnwire.NetAddress, perm bool) error

	DisconnectPeer func(pubKey *btcec.PublicKey) error

	FindPeerByPubStr func(pubStr string) bool

	DB *channeldb.DB

	timer *time.Ticker

	wg sync.WaitGroup

	mu sync.Mutex

	quit chan struct{}
}

type HopTableEntry struct {
	upperHop RouterID

	dis int

	capacity int64

	updated bool
}

// 只能以线程形式启动
func (r *HulaRouter) start() {
	r.wg.Add(1)
	defer r.wg.Done()

	for {
		select {
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

func (r *HulaRouter) findPath() {

}

func (r *HulaRouter) handleProbe(probe *HULAProbeMsg) {

}

func (r *HulaRouter) handleRequest(req *HULARequestMsg) {

}

func (r *HulaRouter) handleResponse(req *HULAResponseMsg) {

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
		RequestPool:      make(map[string]chan lnwire.RIPResponse),
		LinkChangeBuff:   make(chan *LinkChange, BufferSize),
		BestHopTable:     make(map[RouterID]*HopTableEntry),
		ProbeUpdateTable: make(map[RouterID]int64),
		Address:          addr,
		mu:               sync.Mutex{},
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
