package lnwire

import (
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
	"io"
	"net"
)

type MultiPathRequest struct {

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
var _ Message = (*HULARequest)(nil)

// Decode deserializes a serialized MultiPathRequest stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (req *MultiPathRequest) Decode(r io.Reader, pver uint32) error {
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

// Encode serializes the target MultiPathRequst into the passed io.Writer
// observing the protocol version specified.
//
func (req *MultiPathRequest) Encode(w io.Writer, pver uint32) error {
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
func (a *MultiPathRequest) MsgType() MessageType {
	return MsgMultiPathRequest
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *MultiPathRequest) MaxPayloadLength(pver uint32) uint32 {
	return 65533
}


