// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"bytes"
	"net"
	"sync"
)

// PreparedMessage caches on the wire representations of a message payload.
// Use PreparedMessage to efficiently send a message payload to multiple
// connections. PreparedMessage is especially useful when compression is used
// because the CPU and memory expensive compression operation can be executed
// once for a given set of compression options.
type PreparedMessage struct {
	messageType uint8
	offset      uint8
	data        []byte
	mu          sync.Mutex
	frames      map[prepareKey]*preparedFrame
}

// prepareKey defines a unique set of options to cache prepared frames in PreparedMessage.
type prepareKey struct {
	isServer         bool
	compress         bool
	compressionLevel int8
}

// preparedFrame contains data in wire representation.
type preparedFrame struct {
	once sync.Once
	data []byte
}

// NewPreparedMessage returns an initialized PreparedMessage. You can then send
// it to connection using WritePreparedMessage method. Valid wire
// representation will be calculated lazily only once for a set of current
// connection options.
func NewPreparedMessage(messageType int, data []byte) (*PreparedMessage, error) {
	pm := &PreparedMessage{
		messageType: uint8(messageType),
		frames:      make(map[prepareKey]*preparedFrame, 1),
		data:        data,
	}

	// Prepare a plain server frame.
	key := prepareKey{isServer: true, compress: false, compressionLevel: DisableCompression}
	_, frameData, err := pm.frame(key)
	if err != nil {
		return nil, err
	}

	// To protect against caller modifying the data argument, remember the data
	// copied to the plain server frame.
	pm.data = frameData
	pm.offset = uint8(len(frameData) - len(data))
	return pm, nil
}

func (pm *PreparedMessage) frame(key prepareKey) (int, []byte, error) {
	pm.mu.Lock()
	frame, ok := pm.frames[key]
	if !ok {
		frame = &preparedFrame{}
		pm.frames[key] = frame
	}
	pm.mu.Unlock()

	var err error
	frame.once.Do(func() {
		// Prepare a frame using a 'fake' connection.
		// TODO: Refactor code in conn.go to allow more direct construction of
		// the frame.
		var nc prepareConn
		if key.isServer && !key.compress {
			// Pre-allocate the full buffer in the simple case
			nc.buf.Grow(len(pm.data[pm.offset:]) + maxFrameHeaderSize)
		}
		c := LeanConn{
			Conn:                        &nc,
			IsServer:                    key.isServer,
			CompressionLevel:            key.compressionLevel,
			NegotiatedPerMessageDeflate: key.compress,
		}
		err = c.WriteMessage(int(pm.messageType), pm.data[pm.offset:])
		frame.data = nc.buf.Bytes()[:nc.buf.Len():nc.buf.Len()]
	})
	return int(pm.messageType), frame.data, err
}

type prepareConn struct {
	buf bytes.Buffer
	net.Conn
}

func (pc *prepareConn) Write(p []byte) (int, error) { return pc.buf.Write(p) }
