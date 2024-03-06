// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	// Frame header byte 0 bits from Section 5.2 of RFC 6455
	finalBit = 1 << 7
	rsv1Bit  = 1 << 6
	rsv2Bit  = 1 << 5
	rsv3Bit  = 1 << 4

	// Frame header byte 1 bits from Section 5.2 of RFC 6455
	maskBit = 1 << 7

	maxFrameHeaderSize         = 2 + 8 + 4 // Fixed header + length + mask
	maxControlFramePayloadSize = 125

	writeWait = time.Second

	defaultReadBufferSize  = 4096
	defaultWriteBufferSize = 4096

	continuationFrame = 0
	noFrame           = -1
)

// Close codes defined in RFC 6455, section 11.7.
const (
	CloseNormalClosure           = 1000
	CloseGoingAway               = 1001
	CloseProtocolError           = 1002
	CloseUnsupportedData         = 1003
	closeReserved                = 1004
	CloseNoStatusReceived        = 1005
	CloseAbnormalClosure         = 1006
	CloseInvalidFramePayloadData = 1007
	ClosePolicyViolation         = 1008
	CloseMessageTooBig           = 1009
	CloseMandatoryExtension      = 1010
	CloseInternalServerErr       = 1011
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseBadGateway              = 1014
	CloseTLSHandshake            = 1015
)

// The message types are defined in RFC 6455, section 11.8.
const (
	// TextMessage denotes a text data message. The text message payload is
	// interpreted as UTF-8 encoded text data.
	TextMessage = 1

	// BinaryMessage denotes a binary data message.
	BinaryMessage = 2

	// CloseMessage denotes a close control message. The optional message
	// payload contains a numeric code and text. Use the FormatCloseMessage
	// function to format a close message payload.
	CloseMessage = 8

	// PingMessage denotes a ping control message. The optional message payload
	// is UTF-8 encoded text.
	PingMessage = 9

	// PongMessage denotes a pong control message. The optional message payload
	// is UTF-8 encoded text.
	PongMessage = 10
)

// ErrCloseSent is returned when the application writes a message to the
// connection after sending a close message.
var ErrCloseSent = errors.New("websocket: close sent")

// ErrReadLimit is returned when reading a message that is larger than the
// read limit set for the connection.
var ErrReadLimit = errors.New("websocket: read limit exceeded")

// netError satisfies the net Error interface.
type netError struct {
	msg       string
	temporary bool
	timeout   bool
}

func (e *netError) Error() string   { return e.msg }
func (e *netError) Temporary() bool { return e.temporary }
func (e *netError) Timeout() bool   { return e.timeout }

// CloseError represents a close message.
type CloseError struct {
	// Code is defined in RFC 6455, section 11.7.
	Code int

	// Text is the optional text payload.
	Text string
}

func (e *CloseError) Error() string {
	s := []byte("websocket: close ")
	s = strconv.AppendInt(s, int64(e.Code), 10)
	switch e.Code {
	case CloseNormalClosure:
		s = append(s, " (normal)"...)
	case CloseGoingAway:
		s = append(s, " (going away)"...)
	case CloseProtocolError:
		s = append(s, " (protocol error)"...)
	case CloseUnsupportedData:
		s = append(s, " (unsupported data)"...)
	case CloseNoStatusReceived:
		s = append(s, " (no status)"...)
	case CloseAbnormalClosure:
		s = append(s, " (abnormal closure)"...)
	case CloseInvalidFramePayloadData:
		s = append(s, " (invalid payload data)"...)
	case ClosePolicyViolation:
		s = append(s, " (policy violation)"...)
	case CloseMessageTooBig:
		s = append(s, " (message too big)"...)
	case CloseMandatoryExtension:
		s = append(s, " (mandatory extension missing)"...)
	case CloseInternalServerErr:
		s = append(s, " (internal server error)"...)
	case CloseTLSHandshake:
		s = append(s, " (TLS handshake error)"...)
	}
	if e.Text != "" {
		s = append(s, ": "...)
		s = append(s, e.Text...)
	}
	return string(s)
}

// IsCloseError returns boolean indicating whether the error is a *CloseError
// with one of the specified codes.
func IsCloseError(err error, codes ...int) bool {
	if e, ok := err.(*CloseError); ok {
		for _, code := range codes {
			if e.Code == code {
				return true
			}
		}
	}
	return false
}

// IsUnexpectedCloseError returns boolean indicating whether the error is a
// *CloseError with a code not in the list of expected codes.
func IsUnexpectedCloseError(err error, expectedCodes ...int) bool {
	if e, ok := err.(*CloseError); ok {
		for _, code := range expectedCodes {
			if e.Code == code {
				return false
			}
		}
		return true
	}
	return false
}

var (
	errWriteTimeout          = &netError{msg: "websocket: write timeout", timeout: true, temporary: true}
	errUnexpectedEOF         = &CloseError{Code: CloseAbnormalClosure, Text: io.ErrUnexpectedEOF.Error()}
	errBadWriteOpCode        = errors.New("websocket: bad write message type")
	errWriteClosed           = errors.New("websocket: write closed")
	errInvalidControlFrame   = errors.New("websocket: invalid control frame")
	errExtraUsedInClientMode = errors.New("websocket: internal error, extra used in client mode")
	errUnreadDataRemaining   = errors.New("websocket: unread data remaining")
	errUnexpectedMessage     = errors.New("websocket: internal error, unexpected text or binary in Reader")
)

// maskRand is an io.Reader for generating mask bytes. The reader is initialized
// to crypto/rand Reader. Tests swap the reader to a math/rand reader for
// reproducible results.
var maskRand = rand.Reader

// newMaskKey returns a new 32 bit value for masking client frames.
func newMaskKey() [4]byte {
	var k [4]byte
	_, _ = io.ReadFull(maskRand, k[:])
	return k
}

func isControl(frameType int) bool {
	return frameType == CloseMessage || frameType == PingMessage || frameType == PongMessage
}

func isData(frameType int) bool {
	return frameType == TextMessage || frameType == BinaryMessage
}

var validReceivedCloseCodes = [16]bool{
	// see http://www.iana.org/assignments/websocket/websocket.xhtml#close-code-number
	true,  // CloseNormalClosure
	true,  // CloseGoingAway
	true,  // CloseProtocolError
	true,  // CloseUnsupportedData
	false, // reserved
	false, // CloseNoStatusReceived
	false, // CloseAbnormalClosure
	true,  // CloseInvalidFramePayloadData
	true,  // ClosePolicyViolation
	true,  // CloseMessageTooBig
	true,  // CloseMandatoryExtension
	true,  // CloseInternalServerErr
	true,  // CloseServiceRestart
	true,  // CloseTryAgainLater
	true,  // CloseBadGateway
	false, // CloseTLSHandshake
}

var closeErrors = [16]CloseError{
	{Code: CloseNormalClosure},
	{Code: CloseGoingAway},
	{Code: CloseProtocolError},
	{Code: CloseUnsupportedData},
	{Code: closeReserved},
	{Code: CloseNoStatusReceived},
	{Code: CloseAbnormalClosure},
	{Code: CloseInvalidFramePayloadData},
	{Code: ClosePolicyViolation},
	{Code: CloseMessageTooBig},
	{Code: CloseMandatoryExtension},
	{Code: CloseInternalServerErr},
	{Code: CloseServiceRestart},
	{Code: CloseTryAgainLater},
	{Code: CloseBadGateway},
	{Code: CloseTLSHandshake},
}

func isValidReceivedCloseCode(code int) bool {
	if code >= CloseNormalClosure && code <= CloseTLSHandshake {
		return validReceivedCloseCodes[code-CloseNormalClosure]
	}
	return code >= 3000 && code <= 4999
}

// BufferPool represents a pool of buffers. The *sync.Pool type satisfies this
// interface.  The type of the value stored in a pool is not specified.
type BufferPool interface {
	// Get gets a value from the pool or returns nil if the pool is empty.
	Get() interface{}
	// Put adds a value to the pool.
	Put(interface{})
}

// writePoolData is the type added to the write buffer pool. This wrapper is
// used to prevent applications from peeking at and depending on the values
// added to the pool.
type writePoolData struct{ buf []byte }

var bulkWriteBufferPool, controlWriteBufferPool sync.Pool

// The Conn type represents a WebSocket connection.
type Conn struct {
	c           LeanConn
	subprotocol string

	// Write fields
	mu              chan struct{} // used as mutex to protect writes to c
	writeBuf        []byte        // frame is constructed in this buffer.
	writeBufferPool BufferPool
	writeBufferSize int
	writeDeadline   time.Time
	writeErr        error

	enableWriteCompression bool
	compressionLevel       int8

	// Read fields
	readErrCount  uint8
	readErr       error
	readRemaining int64
	handlePong    func(string) error
	handlePing    func(string) error
	handleClose   func(int, string) error
}

func newConn(conn net.Conn, isServer bool, readBufferSize, writeBufferSize int, writeBufferPool BufferPool, br *bufio.Reader, writeBuf []byte) *Conn {

	if br == nil {
		if readBufferSize == 0 {
			readBufferSize = defaultReadBufferSize
		} else if readBufferSize < maxControlFramePayloadSize {
			// must be large enough for control frame
			readBufferSize = maxControlFramePayloadSize
		}
		br = bufio.NewReaderSize(conn, readBufferSize)
	}

	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	writeBufferSize += maxFrameHeaderSize

	if writeBuf == nil && writeBufferPool == nil {
		writeBuf = make([]byte, writeBufferSize)
	}

	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	c := &Conn{
		c: LeanConn{
			IsServer:                          isServer,
			BR:                                br,
			Conn:                              conn,
			NegotiatedPerMessageDeflate:       false,
			CompressionLevel:                  defaultCompressionLevel,
			ReadLimit:                         -1,
			ReturnEarlierAfterPingPongMessage: true,
		},
		mu:                     mu,
		writeBuf:               writeBuf,
		writeBufferPool:        writeBufferPool,
		writeBufferSize:        writeBufferSize,
		enableWriteCompression: true,
		compressionLevel:       defaultCompressionLevel,
	}
	return c
}

// Subprotocol returns the negotiated protocol for the connection.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

// Close closes the underlying network connection without sending or waiting
// for a close message.
func (c *Conn) Close() error {
	return c.c.Close()
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.c.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.c.RemoteAddr()
}

// Write methods

func (c *Conn) writeFatal(err error) error {
	if c.writeErr == nil && err != errWriteClosed {
		c.writeErr = err
	}
	return err
}

// WriteControl writes a control message with the given deadline. The allowed
// message types are CloseMessage, PingMessage and PongMessage.
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if !isControl(messageType) {
		return errBadWriteOpCode
	}
	if len(data) > maxControlFramePayloadSize {
		return errInvalidControlFrame
	}

	if deadline.IsZero() {
		// No timeout for zero time.
		<-c.mu // take ownership
	} else {
		d := time.Until(deadline)
		if d < 0 {
			return errWriteTimeout
		}
		select {
		case <-c.mu: // take ownership
		default:
			// Another write is pending, wait with timeout
			timer := time.NewTimer(d)
			select {
			case <-c.mu: // take ownership
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				return errWriteTimeout
			}
		}
	}

	defer func() { c.mu <- struct{}{} }() // return ownership

	if err := c.writeErr; err != nil {
		return err
	}

	if err := c.c.SetWriteDeadline(deadline); err != nil {
		return c.writeFatal(err)
	}
	err1 := c.c.WriteControl(messageType, data)
	err2 := c.c.SetWriteDeadline(c.writeDeadline)
	if err1 != nil {
		return c.writeFatal(err1)
	}
	if messageType == CloseMessage {
		_ = c.writeFatal(ErrCloseSent)
	} else if err2 != nil {
		return c.writeFatal(err2)
	}
	return nil
}

// NextWriter returns a writer for the next message to send. The writer's Close
// method flushes the complete message to the network.
//
// There can be at most one open writer on a connection. NextWriter closes the
// previous writer if the application has not already done so.
//
// All message types (TextMessage, BinaryMessage, CloseMessage, PingMessage and
// PongMessage) are supported.
func (c *Conn) NextWriter(messageType int) (io.WriteCloser, error) {
	<-c.mu                                // take ownership
	defer func() { c.mu <- struct{}{} }() // return ownership

	if err := c.writeErr; err != nil {
		return nil, err
	}

	w, err := c.c.nextWriter(messageType)
	if err != nil {
		return nil, err
	}
	w.writeBuf = c.writeBuf
	w.wp = c.writeBufferPool
	w.wpSize = c.writeBufferSize
	w.initWriteBuf()
	if w.compress {
		return &messageWriter{
			c: c,
			w: compressNoContextTakeover(&w, w.c.CompressionLevel),
		}, nil
	}
	return &messageWriter{c: c, w: &w}, nil
}

type messageWriter struct {
	c *Conn
	w io.WriteCloser
}

func (w *messageWriter) Write(p []byte) (int, error) {
	<-w.c.mu                                // take ownership
	defer func() { w.c.mu <- struct{}{} }() // return ownership

	if err := w.c.writeErr; err != nil {
		return 0, err
	}
	n, err := w.w.Write(p)
	if err != nil {
		return n, w.c.writeFatal(err)
	}
	return n, nil
}

func (w *messageWriter) WriteString(p string) (int, error) {
	<-w.c.mu                                // take ownership
	defer func() { w.c.mu <- struct{}{} }() // return ownership

	if err := w.c.writeErr; err != nil {
		return 0, err
	}
	n, err := io.WriteString(w.w, p)
	if err != nil {
		return n, w.c.writeFatal(err)
	}
	return n, nil
}

func (w *messageWriter) ReadFrom(r io.Reader) (nn int64, err error) {
	<-w.c.mu // take ownership

	if err = w.c.writeErr; err != nil {
		w.c.mu <- struct{}{} // return ownership
		return 0, err
	}

	if lr, ok := w.w.(*leanMessageWriter); ok {
		nn, err = lr.ReadFrom(r)
		if err != nil {
			err = w.c.writeFatal(err)
		}
		w.c.mu <- struct{}{} // return ownership
		return nn, err
	}
	w.c.mu <- struct{}{} // return ownership

	wpd, ok := bulkWriteBufferPool.Get().(*writePoolData)
	if !ok {
		wpd = &writePoolData{buf: make([]byte, maxFrameHeaderSize+defaultWriteBufferSize)}
	}
	defer bulkWriteBufferPool.Put(wpd)
	buf := wpd.buf
	done := false
	var n int
	for !done {
		n, err = r.Read(buf)
		nn += int64(n)
		if err != nil {
			if err == io.EOF {
				done = true
			} else {
				break
			}
		}

		_, err = w.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return nn, err
}

func (w *messageWriter) Close() error {
	<-w.c.mu                                // take ownership
	defer func() { w.c.mu <- struct{}{} }() // return ownership

	if err := w.c.writeErr; err != nil {
		return err
	}
	if err := w.w.Close(); err != nil {
		return w.c.writeFatal(err)
	}
	return nil
}

// WritePreparedMessage writes prepared message into connection.
func (c *Conn) WritePreparedMessage(pm *PreparedMessage) error {
	<-c.mu                                // take ownership
	defer func() { c.mu <- struct{}{} }() // return ownership
	return c.c.WritePreparedMessage(pm)
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	if c.c.IsServer && c.c.shouldCompress(messageType) {
		<-c.mu                                // take ownership
		defer func() { c.mu <- struct{}{} }() // return ownership
		return c.c.WriteMessage(messageType, data)
	}
	w, err := c.NextWriter(messageType)
	if err != nil {
		return err
	}
	_, err1 := w.Write(data)
	err2 := w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// SetWriteDeadline sets the write deadline on the underlying network
// connection. After a write has timed out, the websocket state is corrupt and
// all future writes will return an error. A zero value for t means writes will
// not time out.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.c.SetDeadline(t)
}

// Read methods

func (c *Conn) handleCloseError(err error) error {
	e, ok := err.(*CloseError)
	if !ok {
		return nil
	}
	if err == errUnexpectedEOF {
		return nil
	}
	return c.CloseHandler()(e.Code, e.Text)
}

func (c *Conn) handleControlMessages(cm []ControlMessage) error {
	for _, m := range cm {
		switch m.MessageType {
		case PingMessage:
			if err := c.PingHandler()(string(m.Payload)); err != nil {
				return err
			}
		case PongMessage:
			if err := c.PongHandler()(string(m.Payload)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Conn) handleProtocolError(message string) error {
	data := FormatCloseMessage(CloseProtocolError, message)
	if len(data) > maxControlFramePayloadSize {
		data = data[:maxControlFramePayloadSize]
	}
	if err := c.WriteControl(CloseMessage, data, time.Now().Add(writeWait)); err != nil {
		return err
	}
	return errors.New("websocket: " + message)
}

// NextReader returns the next data message received from the peer. The
// returned messageType is either TextMessage or BinaryMessage.
//
// There can be at most one open reader on a connection. NextReader discards
// the previous message if the application has not already consumed it.
//
// Applications must break out of the application's read loop when this method
// returns a non-nil error value. Errors returned from this method are
// permanent. Once this method returns a non-nil error, all subsequent calls to
// this method return the same error.
func (c *Conn) NextReader() (messageType int, r io.Reader, err error) {
	for c.readErr == nil {
		if c.readRemaining > 0 {
			n, err := io.CopyN(io.Discard, c.c.BR, c.readRemaining)
			c.readRemaining -= n
			if err != nil {
				c.readErr = err
				_ = c.handleCloseError(err)
				continue
			}
		}

		t, r, err := c.c.NextReaderAny()
		if err != nil {
			c.readErr = err
			_ = c.handleCloseError(err)
			continue
		}
		switch t {
		case BinaryMessage, TextMessage:
			c.readRemaining = r.getReadRemaining()
			return t, &messageReader{c: c, r: r}, nil
		case PingMessage, PongMessage:
			p, err := r.readControlMessagePayload()
			if err != nil {
				c.readErr = err
				continue
			}
			switch t {
			case PingMessage:
				err = c.PingHandler()(string(p))
			case PongMessage:
				err = c.PongHandler()(string(p))
			}
			if err != nil {
				c.readErr = err
				continue
			}
		}
	}

	// Applications that do handle the error returned from this method spin in
	// tight loop on connection failure. To help application developers detect
	// this error, panic on repeated reads to the failed connection.
	c.readErrCount++
	if c.readErrCount == 255 {
		panic("repeated read on failed websocket connection")
	}

	return noFrame, nil, c.readErr
}

type messageReader struct {
	c *Conn
	r MessageReader
}

func (r *messageReader) Read(b []byte) (int, error) {
	for {
		n, err := r.r.Read(b)
		r.c.readRemaining = r.r.getReadRemaining()
		if err != nil {
			if r.c.readRemaining == 0 && err == io.EOF {
				return n, err
			}
			r.c.readErr = err
			_ = r.c.handleCloseError(err)
			return n, err
		}
		if err = r.c.handleControlMessages(r.r.ReceiveControlMessages()); err != nil {
			return n, err
		}
		if n == 0 {
			continue
		}
		return n, nil
	}
}

func (r *messageReader) ReadByte() (byte, error) {
	var b [1]byte
	for {
		if n, err := r.Read(b[:]); err != nil {
			return 0, err
		} else if n == 1 {
			break
		}
	}
	return b[0], nil
}

func (r *messageReader) Close() error {
	err := r.r.Close()
	r.c.readRemaining = r.r.getReadRemaining()
	return err
}

// ReadMessage is a helper method for getting a reader using NextReader and
// reading from that reader to a buffer.
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	messageType, r, err := c.NextReader()
	if err != nil {
		return messageType, nil, err
	}
	p, err = io.ReadAll(r)
	err2 := r.(io.Closer).Close()
	if err != nil {
		return messageType, p, err
	}
	return messageType, p, err2
}

// SetReadDeadline sets the read deadline on the underlying network connection.
// After a read has timed out, the websocket connection state is corrupt and
// all future reads will return an error. A zero value for t means reads will
// not time out.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.c.SetReadDeadline(t)
}

// SetReadLimit sets the maximum size in bytes for a message read from the peer. If a
// message exceeds the limit, the connection sends a close message to the peer
// and returns ErrReadLimit to the application.
func (c *Conn) SetReadLimit(limit int64) {
	c.c.ReadLimit = limit
}

// CloseHandler returns the current close handler
func (c *Conn) CloseHandler() func(code int, text string) error {
	if c.handleClose == nil {
		return c.defaultHandleClose
	}
	return c.handleClose
}

func (c *Conn) defaultHandleClose(code int, text string) error {
	message := FormatCloseMessage(code, "")
	_ = c.WriteControl(CloseMessage, message, time.Now().Add(writeWait))
	return nil
}

// SetCloseHandler sets the handler for close messages received from the peer.
// The code argument to h is the received close code or CloseNoStatusReceived
// if the close message is empty. The default close handler sends a close
// message back to the peer.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// close messages as described in the section on Control Messages above.
//
// The connection read methods return a CloseError when a close message is
// received. Most applications should handle close messages as part of their
// normal error handling. Applications should only set a close handler when the
// application must perform some action before sending a close message back to
// the peer.
func (c *Conn) SetCloseHandler(h func(code int, text string) error) {
	c.handleClose = h
}

// PingHandler returns the current ping handler
func (c *Conn) PingHandler() func(appData string) error {
	if c.handlePing == nil {
		return c.defaultHandlePing
	}
	return c.handlePing
}

func (c *Conn) defaultHandlePing(message string) error {
	err := c.WriteControl(PongMessage, []byte(message), time.Now().Add(writeWait))
	if err == ErrCloseSent {
		return nil
	} else if _, ok := err.(net.Error); ok {
		return nil
	}
	return err
}

// SetPingHandler sets the handler for ping messages received from the peer.
// The appData argument to h is the PING message application data. The default
// ping handler sends a pong to the peer.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// ping messages as described in the section on Control Messages above.
func (c *Conn) SetPingHandler(h func(appData string) error) {
	c.handlePing = h
}

// PongHandler returns the current pong handler
func (c *Conn) PongHandler() func(appData string) error {
	if c.handlePong == nil {
		return c.defaultHandlePong
	}
	return c.handlePong
}

func (c *Conn) defaultHandlePong(string) error {
	return nil
}

// SetPongHandler sets the handler for pong messages received from the peer.
// The appData argument to h is the PONG message application data. The default
// pong handler does nothing.
//
// The handler function is called from the NextReader, ReadMessage and message
// reader Read methods. The application must read the connection to process
// pong messages as described in the section on Control Messages above.
func (c *Conn) SetPongHandler(h func(appData string) error) {
	c.handlePong = h
}

// NetConn returns the underlying connection that is wrapped by c.
// Note that writing to or reading from this connection directly will corrupt the
// WebSocket connection.
func (c *Conn) NetConn() net.Conn {
	return c.c.Conn
}

// UnderlyingConn returns the internal net.Conn. This can be used to further
// modifications to connection specific flags.
// Deprecated: Use the NetConn method.
func (c *Conn) UnderlyingConn() net.Conn {
	return c.c.Conn
}

// EnableWriteCompression enables and disables write compression of
// subsequent text and binary messages. This function is a noop if
// compression was not negotiated with the peer.
func (c *Conn) EnableWriteCompression(enable bool) {
	c.enableWriteCompression = enable
	if enable {
		c.c.CompressionLevel = c.compressionLevel
	} else {
		c.c.CompressionLevel = DisableCompression
	}
}

// SetCompressionLevel sets the flate compression level for subsequent text and
// binary messages. This function is a noop if compression was not negotiated
// with the peer. See the compress/flate package for a description of
// compression levels.
func (c *Conn) SetCompressionLevel(level int) error {
	if !isValidCompressionLevel(level) {
		return errors.New("websocket: invalid compression level")
	}
	c.compressionLevel = int8(level)
	if c.enableWriteCompression {
		c.c.CompressionLevel = c.compressionLevel
	} else {
		c.c.CompressionLevel = DisableCompression
	}
	return nil
}

// FormatCloseMessage formats closeCode and text as a WebSocket close message.
// An empty message is returned for code CloseNoStatusReceived.
func FormatCloseMessage(closeCode int, text string) []byte {
	if closeCode == CloseNoStatusReceived {
		// Return empty message because it's illegal to send
		// CloseNoStatusReceived. Return non-nil value in case application
		// checks for nil.
		return []byte{}
	}
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(closeCode))
	copy(buf[2:], text)
	return buf
}

var messageTypes = map[int]string{
	TextMessage:   "TextMessage",
	BinaryMessage: "BinaryMessage",
	CloseMessage:  "CloseMessage",
	PingMessage:   "PingMessage",
	PongMessage:   "PongMessage",
}

func FormatMessageType(mt int) string {
	return messageTypes[mt]
}
