package hula

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/btcec"
	"net"
	"sync"
	"time"
)

type RouterID [33]byte

type HulaRouter struct {
	SelfNode RouterID

	Address []net.Addr

	Neighbours map[RouterID]struct{}

	ProbeUpdateTable map[RouterID]int64

	BestHopTable     map[RouterID]*HopTableEntry
	SendToPeer       func(target *btcec.PublicKey, msgs ...lnwire.Message) error
	ConnectToPeer    func(addr *lnwire.NetAddress, perm bool) error
	DisconnectPeer   func(pubKey *btcec.PublicKey) error
	FindPeerByPubStr func(pubStr string) bool
	DB               *channeldb.DB
	timer            *time.Ticker

	wg sync.WaitGroup

	quit chan struct{}
}

type HopTableEntry struct {
	upperHop RouterID

	dis int

	capacity int64

	updated bool
}

func start() {

}

func stop() {

}
