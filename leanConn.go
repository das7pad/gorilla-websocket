package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const DisableCompression = -3

type ShouldCloseError struct {
	s    []string
	Code int
}

func (e *ShouldCloseError) Error() string {
	err := CloseError{Code: e.Code, Text: strings.Join(e.s, ", ")}
	return err.Error()
}

func newCloseError(r *leanMessageReader) *CloseError {
	closeCode := CloseNoStatusReceived
	closeText := ""
	if r.readRemaining >= 2 {
		payload, _ := r.readControlMessagePayload()
		if len(payload) >= 2 {
			closeCode = int(binary.BigEndian.Uint16(payload))
			if !isValidReceivedCloseCode(closeCode) {
				closeText = "bad close code " + strconv.Itoa(closeCode)
			} else if len(payload) > 2 {
				closeText = string(payload[2:])
				if !utf8.ValidString(closeText) {
					closeText = "invalid utf8 payload in close frame"
				}
			}
		}
	}
	if closeCode >= CloseNormalClosure && closeCode <= CloseTLSHandshake && len(closeText) == 0 {
		return &closeErrors[closeCode-CloseNormalClosure]
	}
	return &CloseError{Code: closeCode, Text: closeText}
}

var errConnAlreadyClosed = errors.New("websocket: connection already closed")

type LeanConn struct {
	// Conn is the underlying network connection
	Conn net.Conn
	// BR is a read buffer that wraps Conn
	BR *bufio.Reader

	// mu protects Conn from concurrent writes
	mu sync.Mutex

	// ReadLimit specifies the size limit for received messages.
	// DisableCompression disables the read limit.
	ReadLimit int64

	// closed de-duplicates Conn.Close calls.
	closed atomic.Bool
	// CompressionLevel specifies the write compression level.
	// Set to DisableCompression to disable write compression.
	CompressionLevel int8
	// IsServer toggles between client and server connection mode.
	IsServer bool
	// NegotiatedPerMessageDeflate enables support for read/write compression.
	NegotiatedPerMessageDeflate bool
	// ReturnEarlierAfterPingPongMessage enables immediate return from
	// MessageReader after reading a PingMessage or PongMessage.
	// Use MessageReader.ReceiveControlMessages to receive them.
	ReturnEarlierAfterPingPongMessage bool
}

func (c *LeanConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		return c.Conn.Close()
	}
	return errConnAlreadyClosed
}

func (c *LeanConn) LocalAddr() net.Addr {
	return c.Conn.LocalAddr()
}

func (c *LeanConn) RemoteAddr() net.Addr {
	return c.Conn.RemoteAddr()
}

func (c *LeanConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(t)
}

func (c *LeanConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

func (c *LeanConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}

var emptyLeanMessageReader = leanMessageReader{readFinal: true}

func (c *LeanConn) fastPathCheckClosing() (int, error) {
	if p, err := c.BR.Peek(2); err != nil {
		if err == io.EOF {
			return noFrame, errUnexpectedEOF
		}
		return noFrame, err
	} else if (c.IsServer && p[0] == 0x88 && p[1] == 0x82) ||
		(!c.IsServer && p[0] == 0x88 && p[1] == 0x02) {
		r := leanMessageReader{c: c, readFinal: true}
		_, err = r.advanceFrame()
		return CloseMessage, err
	}
	return 0, nil
}

func (c *LeanConn) NextReader() (int, MessageReader, error) {
	if t, err := c.fastPathCheckClosing(); err != nil {
		return t, &emptyLeanMessageReader, err
	}

	r := leanMessageReader{c: c, readFinal: true}
	for {
		t, err := r.advanceFrame()
		if err != nil {
			return t, &r, err
		}
		switch t {
		case BinaryMessage, TextMessage:
			if r.readDecompress {
				return t, decompressNoContextTakeover(&r), nil
			}
			return t, &r, nil
		default:
			if err = r.addControlMessage(t); err != nil {
				return t, &r, err
			}
		}
	}
}

func (c *LeanConn) NextReadIntoBuffer(d *bytes.Buffer) (int64, []ControlMessage, error) {
	if c.NegotiatedPerMessageDeflate {
		_, r, err := c.NextReader()
		if err != nil {
			return 0, r.ReceiveControlMessages(), err
		}
		n, err := d.ReadFrom(r) // needs alloc for r, might be compressed.
		return n, r.ReceiveControlMessages(), err
	}

	if _, err := c.fastPathCheckClosing(); err != nil {
		return 0, nil, err
	}
	r := leanMessageReader{c: c, readFinal: true}
	for {
		t, err := r.advanceFrame()
		if err != nil {
			return 0, r.controlMessages, err
		}
		switch t {
		case BinaryMessage, TextMessage:
			for err == nil && !(r.readRemaining == 0 && r.readFinal) {
				n := int(r.readRemaining)
				d.Grow(n)
				p := d.AvailableBuffer()
				n, err = r.Read(p[:cap(p)])
				d.Write(p[:n])
			}
			err2 := r.Close()
			if err != nil {
				return r.readLength, r.controlMessages, err
			}
			return r.readLength, r.controlMessages, err2
		default:
			if err = r.addControlMessage(t); err != nil {
				return 0, r.controlMessages, err
			}
		}
	}
}

func (c *LeanConn) NextReaderAny() (int, MessageReader, error) {
	if t, err := c.fastPathCheckClosing(); err != nil {
		return t, &emptyLeanMessageReader, err
	}

	r := leanMessageReader{c: c, readFinal: true}
	t, err := r.advanceFrame()
	if err != nil {
		return t, nil, err
	}
	if r.readDecompress {
		return t, decompressNoContextTakeover(&r), nil
	}
	return t, &r, nil
}

// ReadMessage is a helper method for getting a reader using NextReader and
// reading from that reader to a buffer.
func (c *LeanConn) ReadMessage() (messageType int, p []byte, err error) {
	messageType, r, err := c.NextReader()
	if err != nil {
		return messageType, nil, err
	}
	p, err1 := io.ReadAll(r)
	err2 := r.Close()
	if err1 != nil {
		return messageType, p, err1
	}
	return messageType, p, err2
}

func (c *LeanConn) nextWriter(messageType int) (leanMessageWriter, error) {
	if !isControl(messageType) && !isData(messageType) {
		return leanMessageWriter{}, errBadWriteOpCode
	}

	w := leanMessageWriter{
		c:         c,
		pos:       maxFrameHeaderSize,
		frameType: int8(messageType),
		compress:  c.shouldCompress(messageType),
	}
	return w, nil
}

func (c *LeanConn) shouldCompress(messageType int) bool {
	if !c.NegotiatedPerMessageDeflate {
		return false
	}
	if c.CompressionLevel == DisableCompression {
		return false
	}
	return !isControl(messageType)
}

func (c *LeanConn) NextWriter(messageType int) (io.WriteCloser, error) {
	w, err := c.nextWriter(messageType)
	if err != nil {
		return nil, err
	}
	if w.compress {
		return compressNoContextTakeover(&w, c.CompressionLevel), nil
	}
	return &w, nil
}

// WriteMessage is a helper method for getting a writer using NextWriter,
// writing the message and closing the writer.
func (c *LeanConn) WriteMessage(messageType int, data []byte) error {
	if c.IsServer && !c.shouldCompress(messageType) {
		// Fast path with no allocations and single frame.
		w, err := c.nextWriter(messageType)
		if err != nil {
			return err
		}
		w.initWriteBuf()
		n := copy(w.writeBuf[w.pos:], data)
		w.pos += n
		data = data[n:]
		err = w.flushFrame(true, data)
		return err
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

// WriteControl writes a control message with the given deadline. The allowed
// message types are CloseMessage, PingMessage and PongMessage.
func (c *LeanConn) WriteControl(messageType int, data []byte) error {
	if !isControl(messageType) {
		return errBadWriteOpCode
	}
	if len(data) > maxControlFramePayloadSize {
		return errInvalidControlFrame
	}

	b0 := byte(messageType) | finalBit
	b1 := byte(len(data))
	if !c.IsServer {
		b1 |= maskBit
	}

	wpd, ok := controlWriteBufferPool.Get().(*writePoolData)
	if !ok {
		wpd = &writePoolData{buf: make([]byte, maxFrameHeaderSize+maxControlFramePayloadSize)}
	}
	defer controlWriteBufferPool.Put(wpd)
	buf := wpd.buf[:0]
	buf = append(buf, b0, b1)

	if c.IsServer {
		buf = append(buf, data...)
	} else {
		key := newMaskKey()
		buf = append(buf, key[:]...)
		buf = append(buf, data...)
		maskBytes(key, 0, buf[6:])
	}

	c.mu.Lock()
	_, err := c.Conn.Write(buf)
	c.mu.Unlock()
	return err
}

// WritePreparedMessage writes prepared message into connection.
func (c *LeanConn) WritePreparedMessage(pm *PreparedMessage) error {
	compress := c.shouldCompress(int(pm.messageType))
	var frameData []byte
	if c.IsServer && !compress {
		frameData = pm.data
	} else {
		cl := c.CompressionLevel
		if !compress {
			cl = DisableCompression
		}
		var err error
		_, frameData, err = pm.frame(prepareKey{
			isServer:         c.IsServer,
			compress:         compress,
			compressionLevel: cl,
		})
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	_, err := c.Conn.Write(frameData)
	c.mu.Unlock()
	return err
}

func (c *LeanConn) SetCompressionLevel(level int) error {
	if !isValidCompressionLevel(level) {
		return errors.New("websocket: invalid compression level")
	}
	c.CompressionLevel = int8(level)
	return nil
}

// WriteJSON writes the JSON encoding of v as a message.
//
// See the documentation for encoding/json Marshal for details about the
// conversion of Go values to JSON.
func (c *LeanConn) WriteJSON(v interface{}) error {
	w, err := c.NextWriter(TextMessage)
	if err != nil {
		return err
	}
	err1 := json.NewEncoder(w).Encode(v)
	err2 := w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// ReadJSON reads the next JSON-encoded message from the connection and stores
// it in the value pointed to by v.
//
// See the documentation for the encoding/json Unmarshal function for details
// about the conversion of JSON to a Go value.
func (c *LeanConn) ReadJSON(v interface{}) error {
	_, r, err := c.NextReader()
	if err != nil {
		return err
	}
	err1 := json.NewDecoder(r).Decode(v)
	err2 := r.Close()
	if err1 != nil {
		if err1 == io.EOF {
			// One value is expected in the message.
			return io.ErrUnexpectedEOF
		}
		return err1
	}
	return err2
}

type MessageReader interface {
	io.ReadCloser
	ReceiveControlMessages() []ControlMessage

	readControlMessagePayload() ([]byte, error)
	getReadRemaining() int64
}

type ControlMessage struct {
	MessageType int
	Payload     []byte
}

type leanMessageReader struct {
	c              *LeanConn
	readRemaining  int64
	readLength     int64
	readMaskKey    [4]byte
	readMaskPos    uint8
	readFinal      bool
	readDecompress bool

	controlMessages []ControlMessage
}

func (r *leanMessageReader) getReadRemaining() int64 {
	return r.readRemaining
}

func (r *leanMessageReader) ReceiveControlMessages() []ControlMessage {
	m := r.controlMessages
	r.controlMessages = nil
	return m
}

func (r *leanMessageReader) readControlMessagePayload() ([]byte, error) {
	payload, err := r.read(int(r.readRemaining))
	r.readRemaining -= int64(len(payload))
	if r.c.IsServer {
		maskBytes(r.readMaskKey, 0, payload)
	}
	return payload, err
}

func (r *leanMessageReader) read(n int) ([]byte, error) {
	p, err := r.c.BR.Peek(n)
	if err != nil && err == io.EOF {
		err = errUnexpectedEOF
	}
	_, err2 := r.c.BR.Discard(len(p))
	if err != nil {
		return p, err
	}
	return p, err2
}

// setReadRemaining tracks the number of bytes remaining on the connection. If n
// overflows, an ErrReadLimit is returned.
func (r *leanMessageReader) setReadRemaining(n int64) error {
	if n < 0 {
		return ErrReadLimit
	}
	r.readRemaining = n
	return nil
}

func (r *leanMessageReader) advanceFrame() (int, error) {
	// 1. Check pending
	if r.readRemaining > 0 {
		return noFrame, errUnreadDataRemaining
	}

	// 2. Read and parse first two bytes of frame header.
	// To aid debugging, collect and report all errors in the first two bytes
	// of the header.

	p, err := r.read(2)
	if err != nil {
		return noFrame, err
	}

	frameType := int(p[0] & 0xf)
	final := p[0]&finalBit != 0
	rsv1 := p[0]&rsv1Bit != 0
	rsv2 := p[0]&rsv2Bit != 0
	rsv3 := p[0]&rsv3Bit != 0
	mask := p[1]&maskBit != 0

	if err = r.setReadRemaining(int64(p[1] & 0x7f)); err != nil {
		return noFrame, err
	}

	var e []string

	r.readDecompress = false
	if rsv1 {
		if r.c.NegotiatedPerMessageDeflate {
			r.readDecompress = true
		} else {
			e = append(e, "RSV1 set")
		}
	}

	if rsv2 {
		e = append(e, "RSV2 set")
	}

	if rsv3 {
		e = append(e, "RSV3 set")
	}

	switch frameType {
	case CloseMessage, PingMessage, PongMessage:
		if r.readRemaining > maxControlFramePayloadSize {
			e = append(e, "len > 125 for control")
		}
		if !final {
			e = append(e, "FIN not set on control")
		}
	case TextMessage, BinaryMessage:
		if !r.readFinal {
			e = append(e, "data before FIN")
		}
		r.readFinal = final
	case continuationFrame:
		if r.readFinal {
			e = append(e, "continuation after FIN")
		}
		r.readFinal = final
	default:
		e = append(e, "bad opcode "+strconv.Itoa(frameType))
	}

	if mask != r.c.IsServer {
		e = append(e, "bad MASK")
	}

	if len(e) > 0 {
		return noFrame, &ShouldCloseError{Code: CloseProtocolError, s: e}
	}

	// 3. Read and parse frame length as per
	// https://tools.ietf.org/html/rfc6455#section-5.2
	//
	// The length of the "Payload data", in bytes: if 0-125, that is the payload
	// length.
	// - If 126, the following 2 bytes interpreted as a 16-bit unsigned
	// integer are the payload length.
	// - If 127, the following 8 bytes interpreted as
	// a 64-bit unsigned integer (the most significant bit MUST be 0) are the
	// payload length. Multibyte length quantities are expressed in network byte
	// order.

	switch r.readRemaining {
	case 126:
		p, err = r.read(2)
		if err != nil {
			return noFrame, err
		}

		if err = r.setReadRemaining(int64(binary.BigEndian.Uint16(p))); err != nil {
			return noFrame, err
		}
	case 127:
		p, err = r.read(8)
		if err != nil {
			return noFrame, err
		}

		if err = r.setReadRemaining(int64(binary.BigEndian.Uint64(p))); err != nil {
			return noFrame, err
		}
	}

	// 4. Handle frame masking.

	if mask {
		r.readMaskPos = 0
		p, err = r.read(4)
		if err != nil {
			return noFrame, err
		}
		copy(r.readMaskKey[:], p)
	}

	// 5. For text and binary messages, enforce read limit and return.

	switch frameType {
	case continuationFrame, TextMessage, BinaryMessage:
		r.readLength += r.readRemaining
		// Don't allow readLength to overflow in the presence of a large readRemaining
		// counter.
		if r.readLength < 0 {
			return noFrame, ErrReadLimit
		}
		if r.c.ReadLimit != -1 && r.readLength > r.c.ReadLimit {
			return noFrame, ErrReadLimit
		}
		return frameType, nil
	case CloseMessage:
		return CloseMessage, newCloseError(r)
	default:
		return frameType, nil
	}
}

func (r *leanMessageReader) fill() error {
	for r.readRemaining == 0 {
		if r.readFinal {
			return io.EOF
		}
		frameType, err := r.advanceFrame()
		if err != nil {
			return err
		}
		switch frameType {
		case continuationFrame:
			return nil
		case TextMessage, BinaryMessage:
			return errUnexpectedMessage
		case PingMessage, PongMessage:
			if err = r.addControlMessage(frameType); err != nil {
				return err
			}
			if r.c.ReturnEarlierAfterPingPongMessage {
				return nil
			}
		case CloseMessage:
			return newCloseError(r)
		}
	}
	return nil
}

func (r *leanMessageReader) ReadByte() (byte, error) {
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

func (r *leanMessageReader) Read(b []byte) (int, error) {
	if err := r.fill(); err != nil || r.readRemaining == 0 {
		return 0, err
	}
	if int64(len(b)) > r.readRemaining {
		b = b[:r.readRemaining]
	}
	n, err := r.c.BR.Read(b)
	if err2 := r.setReadRemaining(r.readRemaining - int64(n)); err2 != nil {
		return 0, err2
	}
	if r.readRemaining > 0 && err == io.EOF {
		return 0, errUnexpectedEOF
	}
	if r.c.IsServer {
		r.readMaskPos = maskBytes(r.readMaskKey, r.readMaskPos, b[:n])
	}
	return n, err
}

func (r *leanMessageReader) addControlMessage(frameType int) error {
	m := ControlMessage{MessageType: frameType}
	if r.readRemaining > 0 {
		payload, err := r.readControlMessagePayload()
		if err != nil {
			return err
		}
		m.Payload = append(m.Payload, payload...)
	}
	r.controlMessages = append(r.controlMessages, m)
	return nil
}

func (r *leanMessageReader) Close() error {
	if r.readRemaining == 0 {
		return nil
	}
	if int64(r.c.BR.Buffered()) < r.readRemaining {
		// Do not read arbitrary amount of data from Close.
		return errUnreadDataRemaining
	}
	n, err := r.c.BR.Discard(int(r.readRemaining))
	r.readRemaining -= int64(n)
	return err
}

type leanMessageWriter struct {
	c             *LeanConn
	writeBuf      []byte
	writePoolData *writePoolData
	wp            BufferPool
	wpSize        int
	pos           int
	frameType     int8
	compress      bool
	wroteFinal    bool
}

func (w *leanMessageWriter) initWriteBuf() {
	if w.writeBuf != nil {
		return
	}
	var ok bool
	if w.wp == nil {
		if w.writePoolData, ok = bulkWriteBufferPool.Get().(*writePoolData); !ok {
			w.writePoolData = &writePoolData{buf: make([]byte, maxFrameHeaderSize+defaultWriteBufferSize)}
		}
	} else {
		if w.writePoolData, ok = w.wp.Get().(*writePoolData); !ok {
			w.writePoolData = &writePoolData{buf: make([]byte, w.wpSize)}
		}
	}
	w.writeBuf = w.writePoolData.buf
}

func (w *leanMessageWriter) Write(p []byte) (int, error) {
	if w.wroteFinal {
		return 0, errWriteClosed
	}
	w.initWriteBuf()

	if len(p) > 2*len(w.writeBuf) && w.c.IsServer {
		// Don't buffer large messages.
		if err := w.flushFrame(false, p); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	nn := len(p)
	for len(p) > 0 {
		n, err := w.nCopy(len(p))
		if err != nil {
			return 0, err
		}
		copy(w.writeBuf[w.pos:], p[:n])
		w.pos += n
		p = p[n:]
	}
	return nn, nil
}

func (w *leanMessageWriter) WriteString(p string) (int, error) {
	if w.wroteFinal {
		return 0, errWriteClosed
	}
	w.initWriteBuf()

	nn := len(p)
	for len(p) > 0 {
		n, err := w.nCopy(len(p))
		if err != nil {
			return 0, err
		}
		copy(w.writeBuf[w.pos:], p[:n])
		w.pos += n
		p = p[n:]
	}
	return nn, nil
}

func (w *leanMessageWriter) ReadFrom(r io.Reader) (nn int64, err error) {
	if w.wroteFinal {
		return 0, errWriteClosed
	}
	w.initWriteBuf()

	for {
		if w.pos == len(w.writeBuf) {
			err = w.flushFrame(false, nil)
			if err != nil {
				break
			}
		}
		var n int
		n, err = r.Read(w.writeBuf[w.pos:])
		w.pos += n
		nn += int64(n)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
	}
	return nn, err
}

func (w *leanMessageWriter) nCopy(max int) (int, error) {
	n := len(w.writeBuf) - w.pos
	if n <= 0 {
		if err := w.flushFrame(false, nil); err != nil {
			return 0, err
		}
		n = len(w.writeBuf) - w.pos
	}
	if n > max {
		n = max
	}
	return n, nil
}

// flushFrame writes buffered data and extra as a frame to the network. The
// final argument indicates that this is the last frame in the message.
func (w *leanMessageWriter) flushFrame(final bool, extra []byte) error {
	if w.wroteFinal {
		return errWriteClosed
	}
	length := w.pos - maxFrameHeaderSize + len(extra)

	// Check for invalid control frames.
	if isControl(int(w.frameType)) &&
		(!final || length > maxControlFramePayloadSize) {
		w.done()
		return errInvalidControlFrame
	}

	b0 := byte(w.frameType)
	if final {
		b0 |= finalBit
	}
	if w.compress {
		b0 |= rsv1Bit
	}
	w.compress = false

	b1 := byte(0)
	if !w.c.IsServer {
		b1 |= maskBit
	}

	// Assume that the frame starts at beginning of w.writeBuf.
	framePos := 0
	if w.c.IsServer {
		// Adjust up if mask not included in the header.
		framePos = 4
	}

	switch {
	case length >= 65536:
		w.writeBuf[framePos] = b0
		w.writeBuf[framePos+1] = b1 | 127
		binary.BigEndian.PutUint64(w.writeBuf[framePos+2:], uint64(length))
	case length > 125:
		framePos += 6
		w.writeBuf[framePos] = b0
		w.writeBuf[framePos+1] = b1 | 126
		binary.BigEndian.PutUint16(w.writeBuf[framePos+2:], uint16(length))
	default:
		framePos += 8
		w.writeBuf[framePos] = b0
		w.writeBuf[framePos+1] = b1 | byte(length)
	}

	if !w.c.IsServer {
		key := newMaskKey()
		copy(w.writeBuf[maxFrameHeaderSize-4:], key[:])
		maskBytes(key, 0, w.writeBuf[maxFrameHeaderSize:w.pos])
		if len(extra) > 0 {
			w.done()
			return errExtraUsedInClientMode
		}
	}

	if err := w.write(w.writeBuf[framePos:w.pos], extra); err != nil {
		w.done()
		return err
	}

	if final || w.frameType == CloseMessage {
		w.done()
		return nil
	}

	// Setup for next frame.
	w.pos = maxFrameHeaderSize
	w.frameType = continuationFrame
	return nil
}

func (w *leanMessageWriter) done() {
	w.wroteFinal = true
	w.writeBuf = nil
	if w.writePoolData != nil {
		if w.wp == nil {
			bulkWriteBufferPool.Put(w.writePoolData)
		} else {
			w.wp.Put(w.writePoolData)
		}
		w.writePoolData = nil
	}
}

func (w *leanMessageWriter) Close() error {
	if w.wroteFinal {
		return errWriteClosed
	}
	w.initWriteBuf()
	return w.flushFrame(true, nil)
}

func (w *leanMessageWriter) write(buf0, buf1 []byte) error {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	if len(buf1) == 0 {
		_, err := w.c.Conn.Write(buf0)
		return err
	}
	if pc, ok := w.c.Conn.(*prepareConn); ok {
		pc.buf.Grow(len(buf0) + len(buf1))
		pc.buf.Write(buf0)
		pc.buf.Write(buf1)
		return nil
	}
	b := net.Buffers{buf0, buf1}
	_, err := b.WriteTo(w.c.Conn)
	return err
}
