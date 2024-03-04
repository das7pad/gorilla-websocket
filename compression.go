// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"compress/flate"
	"errors"
	"io"
	"sync"
)

const (
	minCompressionLevel     = -2 // flate.HuffmanOnly not defined in Go < 1.6
	maxCompressionLevel     = flate.BestCompression
	defaultCompressionLevel = 1
)

var (
	flateWriterPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	flateReaderPool  = sync.Pool{New: func() interface{} {
		return flate.NewReader(nil)
	}}
)

const flateReadTail =
// Add four bytes as specified in RFC
"\x00\x00\xff\xff" +
	// Add final block to squelch unexpected EOF error from flate reader.
	"\x01\x00\x00\xff\xff"

func decompressNoContextTakeover(r *messageReader) *flateReadWrapper {
	f := flateReadWrapper{
		fr:              flateReaderPool.Get().(io.ReadCloser),
		flateReadSource: flateReadSource{r: r, i: -1},
	}
	if err := f.fr.(flate.Resetter).Reset(&f.flateReadSource, nil); err != nil {
		panic(err)
	}
	return &f
}

type flateReadSource struct {
	r *messageReader
	i int
}

func (f *flateReadSource) Read(p []byte) (n int, err error) {
	switch f.i {
	case -1:
		n, err = f.r.Read(p)
		if err != nil && err == io.EOF {
			f.i = 0
			return n, nil
		}
		return n, err
	case len(flateReadTail):
		return 0, io.EOF
	default:
		n = copy(p, flateReadTail[f.i:])
		f.i += n
		return n, nil
	}
}

func (f *flateReadSource) ReadByte() (byte, error) {
	switch f.i {
	case -1:
		b, err := f.r.ReadByte()
		if err != nil && err == io.EOF {
			f.i = 1
			return flateReadTail[0], nil
		}
		return b, err
	case len(flateReadTail):
		return 0, io.EOF
	default:
		c := flateReadTail[f.i]
		f.i++
		return c, nil
	}
}

func isValidCompressionLevel(level int) bool {
	return minCompressionLevel <= level && level <= maxCompressionLevel
}

func compressNoContextTakeover(w io.WriteCloser, level int8) io.WriteCloser {
	p := &flateWriterPools[level-minCompressionLevel]
	tw := &truncWriter{w: w}
	fw, _ := p.Get().(*flate.Writer)
	if fw == nil {
		fw, _ = flate.NewWriter(tw, int(level))
	} else {
		fw.Reset(tw)
	}
	return &flateWriteWrapper{fw: fw, tw: tw, p: p}
}

// truncWriter is an io.Writer that writes all but the last four bytes of the
// stream to another io.Writer.
type truncWriter struct {
	w io.WriteCloser
	n int
	p [4]byte
}

func (w *truncWriter) Write(p []byte) (int, error) {
	n := 0
	if (w.n > 0 || len(p) < len(w.p)) && w.n < len(w.p) {
		n = copy(w.p[w.n:], p)
		p = p[n:]
		w.n += n
	}
	if len(p) == 0 {
		return n, nil
	}
	m := len(p) - len(w.p)
	if m < 0 {
		nn, err := w.w.Write(w.p[:len(p)])
		w.n -= nn
		copy(w.p[:], w.p[nn:])
		if err != nil {
			return n, err
		}
		nn = copy(w.p[w.n:], p)
		w.n += nn
		return n + nn, nil
	}
	if w.n > 0 {
		nn, err := w.w.Write(w.p[:])
		w.n -= nn
		if w.n > 0 {
			copy(w.p[:], w.p[nn:])
		}
		if err != nil {
			return n, err
		}
	}
	if m > 0 {
		nn, err := w.w.Write(p[:m])
		n += nn
		if err != nil {
			return n, err
		}
		p = p[nn:]
	}
	w.n += copy(w.p[w.n:], p)
	n += w.n
	return n, nil
}

type flateWriteWrapper struct {
	fw *flate.Writer
	tw *truncWriter
	p  *sync.Pool
}

func (w *flateWriteWrapper) Write(p []byte) (int, error) {
	if w.fw == nil {
		return 0, errWriteClosed
	}
	return w.fw.Write(p)
}

func (w *flateWriteWrapper) Close() error {
	if w.fw == nil {
		return errWriteClosed
	}
	err1 := w.fw.Flush()
	w.p.Put(w.fw)
	w.fw = nil
	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	err2 := w.tw.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

type flateReadWrapper struct {
	fr io.ReadCloser
	flateReadSource
}

func (r *flateReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := r.fr.Read(p)
	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		_ = r.Close()
	}
	return n, err
}

func (r *flateReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}
	err := r.fr.Close()
	flateReaderPool.Put(r.fr)
	r.fr = nil
	return err
}
