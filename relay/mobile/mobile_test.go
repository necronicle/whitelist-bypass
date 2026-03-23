package mobile

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	t.Parallel()

	buf := make([]byte, 5+32)
	payload := []byte("hello")

	n := encodeFrameInto(buf, 42, msgData, payload)
	connID, msgType, decoded := decodeFrame(buf[:n])

	if connID != 42 {
		t.Fatalf("unexpected connID: got %d want 42", connID)
	}
	if msgType != msgData {
		t.Fatalf("unexpected msgType: got %d want %d", msgType, msgData)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("unexpected payload: got %q want %q", decoded, payload)
	}
}

func TestDecodeFrameRejectsShortFrames(t *testing.T) {
	t.Parallel()

	connID, msgType, payload := decodeFrame([]byte{1, 2, 3, 4})
	if connID != 0 || msgType != 0 || payload != nil {
		t.Fatalf("short frame should decode to zero values, got connID=%d msgType=%d payload=%v", connID, msgType, payload)
	}
}

func TestJoinerHandleMessageForwardsUDPReplyWithoutTCPConn(t *testing.T) {
	t.Parallel()

	loopback := net.ParseIP("127.0.0.1")

	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback, Port: 0})
	if err != nil {
		t.Fatalf("listen udp server: %v", err)
	}
	defer serverConn.Close()

	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback, Port: 0})
	if err != nil {
		t.Fatalf("listen udp client: %v", err)
	}
	defer clientConn.Close()

	header := []byte{0x00, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0x01, 0xbb}
	payload := []byte("dns-reply")

	j := &joinerRelay{}
	j.udpClients.Store(uint32(7), &udpClient{
		udpConn:     serverConn,
		clientAddr:  clientConn.LocalAddr().(*net.UDPAddr),
		socksHeader: header,
	})

	j.handleMessage(7, msgUDPReply, payload)

	buf := make([]byte, 128)
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	n, _, err := clientConn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read udp reply: %v", err)
	}

	want := append(append([]byte(nil), header...), payload...)
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("unexpected udp reply: got %x want %x", buf[:n], want)
	}

	if _, ok := j.udpClients.Load(uint32(7)); ok {
		t.Fatal("udp client should be removed after reply is forwarded")
	}
}
