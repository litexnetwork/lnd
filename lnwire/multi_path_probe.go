package lnwire

import (
	"github.com/roasbeef/btcutil"
	"io"
)

// MultiPathProbe is the probe message which will be sent by every node periodically.
// This message will indicate neighbors to update their MultiPathRouter's config.
type MultiPathProbe struct {
	Destination [33]byte

	UpperHop [33]byte

	Distance uint8

	Capacity btcutil.Amount
}

// Decode deserializes a serialized MultiPathProbe message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *MultiPathProbe) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&c.Destination,
		&c.UpperHop,
		&c.Distance,
		&c.Capacity,
	)
}

// Encode serializes the target MultiPathProbe into the passed io.Writer
// observing the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (c *MultiPathProbe) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		c.Destination,
		c.UpperHop,
		c.Distance,
		c.Capacity,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (c *MultiPathProbe) MsgType() MessageType {
	return MsgMultiPathProbe
}

// MaxPayloadLength returns the maximum allowed payload size for an MultiPathProbe
// complete message observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (c *MultiPathProbe) MaxPayloadLength(uint32) uint32 {
	// 33 + 33 + 1 + 8
	return 75
}
