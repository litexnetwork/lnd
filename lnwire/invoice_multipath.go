package lnwire

import (
	"io"
	"github.com/roasbeef/btcutil"
)

type PaymentRequest []byte
type InvoiceRequest struct {
	RequestID [33]byte
	PathNum   uint8
	Amount    btcutil.Amount
}

type InvoiceResponse struct {
	RequestID [33]byte
	PayReqs   []PaymentRequest
	Error     ErrorData
}

// Decode deserializes a serialized InvoiceRequest stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (req *InvoiceRequest) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&req.RequestID,
		&req.PathNum,
		&req.Amount,
	)
}

// Encode serializes the target InvoiceRequest into the passed io.Writer
// observing the protocol version specified.
//
func (req *InvoiceRequest) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		req.RequestID,
		req.PathNum,
		req.Amount,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *InvoiceRequest) MsgType() MessageType {
	return MsgInvoiceRequest
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *InvoiceRequest) MaxPayloadLength(pver uint32) uint32 {
	return 65533
}

// Decode deserializes a serialized InvoiceRequest stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (req *InvoiceResponse) Decode(r io.Reader, pver uint32) error {
	return readElements(r,
		&req.RequestID,
		&req.PayReqs,
		&req.Error,
	)
}

// Encode serializes the target InvoiceRequest into the passed io.Writer
// observing the protocol version specified.
//
func (req *InvoiceResponse) Encode(w io.Writer, pver uint32) error {
	return writeElements(w,
		req.RequestID,
		req.PayReqs,
		req.Error,
	)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (a *InvoiceResponse) MsgType() MessageType {
	return MsgInvoiceResponse
}

// MaxPayloadLength returns the maximum allowed payload size for this message
// observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (a *InvoiceResponse) MaxPayloadLength(pver uint32) uint32 {
	return 65533
}
