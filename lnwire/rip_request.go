package lnwire

import (
	"net"
	"github.com/btcsuite/btcutil"
	"io"
	"github.com/roasbeef/btcd/wire"
)

type RIPRequest struct {

	// SourceNodeID indicates the source node identity.
	SourceNodeID [33]byte

	// Address includes two specification fields: 'ipv6' and 'port' on
	// which the node is accepting incoming connections.
	Addresses []net.Addr

	// RequestID is the id of request.
	RequestID [33]byte

	// DestNodeID is the identity of destination node
	DestNodeID [33]byte

	// RequestAmount indicates the amount we requested.
	RequestAmount btcutil.Amount

	// PathNodes indicates the nodes of the path we find.
	PathNodes [][33]byte

	// PathChannls indicates the channels of the path we find.
	PathChannels []wire.OutPoint
}

// A compile time check to ensure NodeAnnouncement implements the
// lnwire.Message interface.
var _ Message = (*RIPRequest)(nil)

// Decode deserializes a serialized RIPRequest stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (req *RIPRequest) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&req.SourceNodeID,
		&req.Addresses,
		&req.RequestID,
		&req.DestNodeID,
		&req.RequestAmount,
		&req.PathNodes,
		&req.PathChannels,
	)
}

// Encode serializes the target RIPRequst into the passed io.Writer
// observing the protocol version specified.
//
func (req *RIPRequest) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		req.SourceNodeID,
		req.Addresses,
		req.RequestID,
		req.DestNodeID,
		req.RequestAmount,
		req.PathNodes,
		req.PathChannels,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *RIPRequest) MsgType() MessageType {
	return MsgRIPRequest
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *RIPRequest) MaxPayloadLength(pver uint32) uint32 {
	return 65533
}
