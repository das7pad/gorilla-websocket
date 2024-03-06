package websocket

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

func TestTruncWriter(t *testing.T) {
	t.Parallel()
	const data = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijlkmnopqrstuvwxyz987654321"
	for n := 1; n <= 10; n++ {
		var b bytes.Buffer
		c := newTestConn(nil, &b, true)
		w := &truncWriter{}
		{
			lw, err := c.c.nextWriter(BinaryMessage)
			if err != nil {
				t.Fatal(err)
			}
			w.w = &lw
		}
		p := []byte(data)
		for len(p) > 0 {
			m := len(p)
			if m > n {
				m = n
			}
			nn, err := w.Write(p[:m])
			if err != nil {
				t.Fatal(err)
			}
			p = p[nn:]
		}
		if err := w.w.Close(); err != nil {
			t.Fatal(err)
		}
		if got, want := b.String()[2:], data[:len(data)-len(w.p)]; got != want {
			t.Errorf("%d: got=%q want=%q", n, got, want)
		}
	}
}

func textMessages(num int) [][]byte {
	messages := make([][]byte, num)
	for i := 0; i < num; i++ {
		msg := fmt.Sprintf("planet: %d, country: %d, city: %d, street: %d", i, i, i, i)
		messages[i] = []byte(msg)
	}
	return messages
}

func BenchmarkWriteNoCompression(b *testing.B) {
	w := io.Discard
	c := newTestConn(nil, w, false)
	messages := textMessages(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WriteMessage(TextMessage, messages[i%len(messages)]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
}

func BenchmarkWriteWithCompression(b *testing.B) {
	w := io.Discard
	c := newTestConn(nil, w, false)
	messages := textMessages(100)
	c.c.NegotiatedPerMessageDeflate = true
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.WriteMessage(TextMessage, messages[i%len(messages)]); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
}

func TestValidCompressionLevel(t *testing.T) {
	t.Parallel()
	c := newTestConn(nil, nil, false)
	for _, level := range []int{minCompressionLevel - 1, maxCompressionLevel + 1} {
		if err := c.SetCompressionLevel(level); err == nil {
			t.Errorf("no error for level %d", level)
		}
	}
	for _, level := range []int{minCompressionLevel, maxCompressionLevel} {
		if err := c.SetCompressionLevel(level); err != nil {
			t.Errorf("error for level %d", level)
		}
	}
}
