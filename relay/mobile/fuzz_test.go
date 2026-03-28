package mobile

import (
	"context"
	"net"
	"testing"
	"time"
)

func FuzzDecodeFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0x04, 'h', 'e', 'l', 'l', 'o'})
	f.Add([]byte{})
	f.Add([]byte{1, 2, 3, 4})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(make([]byte, 5))

	f.Fuzz(func(t *testing.T, data []byte) {
		connID, msgType, payload := decodeFrame(data)
		if len(data) < 5 {
			if connID != 0 || msgType != 0 || payload != nil {
				t.Fatal("short frame should decode to zero values")
			}
			return
		}
		// Verify round-trip for valid frames.
		buf := make([]byte, 5+len(payload))
		n := encodeFrameInto(buf, connID, msgType, payload)
		connID2, msgType2, payload2 := decodeFrame(buf[:n])
		if connID != connID2 || msgType != msgType2 {
			t.Fatalf("round-trip failed: %d/%d vs %d/%d", connID, msgType, connID2, msgType2)
		}
		if len(payload) != len(payload2) {
			t.Fatalf("round-trip payload length: %d vs %d", len(payload), len(payload2))
		}
	})
}

func FuzzSOCKS5Greeting(f *testing.F) {
	f.Add([]byte{0x05, 0x01, 0x00})
	f.Add([]byte{0x05, 0x00})
	f.Add([]byte{0x04, 0x01, 0x00})
	f.Add([]byte{0x05, 0x01, 0x00, 0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		m := &Metrics{}
		j := &joinerRelay{
			ready:   make(chan struct{}),
			metrics: m,
		}
		close(j.ready)

		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()

		done := make(chan struct{})
		go func() {
			defer close(done)
			j.handleSOCKS(context.Background(), server)
		}()

		client.SetDeadline(time.Now().Add(100 * time.Millisecond))
		client.Write(data)

		// Read any response, ignore errors.
		buf := make([]byte, 64)
		client.Read(buf)
		client.Close()

		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Fatal("handleSOCKS did not return in time")
		}
	})
}
