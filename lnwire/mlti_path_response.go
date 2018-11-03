package lnwire

import (
	"github.com/roasbeef/btcd/wire"
	"io"
)

type MultiPathResponse struct {
	RequestID [33]byte

	PathChannels []wire.OutPoint

	PathNodes [][33]byte

	Success uint8
}

// Decode deserializes a serialized MultiPathResponse stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (req *MultiPathResponse) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&req.RequestID,
		&req.PathChannels,
		&req.PathNodes,
		&req.Success,
	)
}

// Encode serializes the target MultiPathResponse into the passed io.Writer
// observing the protocol version specified.
//
func (req *MultiPathResponse) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		req.RequestID,
		req.PathChannels,
		req.PathNodes,
		req.Success,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *MultiPathResponse) MsgType() MessageType {
	return MsgMultiPathResponse
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *MultiPathResponse) MaxPayloadLength(pver uint32) uint32 {
	return 65533
}