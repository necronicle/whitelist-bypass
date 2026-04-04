package mobile

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMaskAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"192.168.1.100:443", "192.168.x.x:443"},
		{"10.0.0.1:80", "10.0.x.x:80"},
		{"192.168.1.100", "192.168.x.x"},
		{"[2001:db8::1]:443", "[x::x]:443"},
		{"2001:db8::1", "x::x"},
		{"example.com:443", "e***:443"},
		{"example.com", "e***"},
		{"a.io:8080", "a***:8080"},
		{"", ""},
	}
	for _, tt := range tests {
		got := maskAddr(tt.input)
		if got != tt.want {
			t.Errorf("maskAddr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMaskHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"192.168.1.100", "192.168.x.x"},
		{"10.0.0.1", "10.0.x.x"},
		{"2001:db8::1", "x::x"},
		{"[::1]", "[x::x]"},
		{"example.com", "e***"},
		{"", ""},
	}
	for _, tt := range tests {
		got := maskHost(tt.input)
		if got != tt.want {
			t.Errorf("maskHost(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

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

	m := &Metrics{}
	j := &joinerRelay{metrics: m}
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

func TestEmptyUDPReplyCleanup(t *testing.T) {
	t.Parallel()

	m := &Metrics{}
	j := &joinerRelay{metrics: m}
	j.udpClients.Store(uint32(9), &udpClient{})

	j.handleMessage(9, msgUDPReply, nil)

	if _, ok := j.udpClients.Load(uint32(9)); ok {
		t.Fatal("udp client should be removed after empty reply")
	}
}

func TestConnectionLimit(t *testing.T) {
	t.Parallel()

	m := &Metrics{}
	m.ActiveConns.Store(maxConns)
	c := &creatorRelay{metrics: m}

	c.connect(1, "127.0.0.1:1")

	if m.ActiveConns.Load() != maxConns {
		t.Fatalf("activeConns changed: got %d want %d", m.ActiveConns.Load(), maxConns)
	}
}

// waitForPort blocks until the given TCP port is accepting connections.
func waitForPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d not ready", port)
}

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// readPastHandshake reads and discards msgHandshake and msgPing frames, returning the first "real" frame.
func readPastControl(t *testing.T, ws *websocket.Conn) (uint32, byte, []byte) {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		connID, mt, payload := decodeFrame(msg)
		if mt == msgHandshake || mt == msgPing || mt == msgPong {
			continue
		}
		return connID, mt, payload
	}
}

func TestCreatorConnectDataClose(t *testing.T) {
	t.Parallel()

	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	echoAddr := echoLn.Addr().String()

	go func() {
		for {
			conn, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	port := freePort(t)
	relay, err := StartCreator(port, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Stop()
	waitForPort(t, port)

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", port)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	// Send msgConnect.
	connID := uint32(1)
	frame := make([]byte, 5+len(echoAddr))
	binary.BigEndian.PutUint32(frame[0:4], connID)
	frame[4] = msgConnect
	copy(frame[5:], echoAddr)
	ws.WriteMessage(websocket.BinaryMessage, frame)

	// Read past handshake, expect msgConnectOK.
	_, mt, _ := readPastControl(t, ws)
	if mt != msgConnectOK {
		t.Fatalf("expected msgConnectOK, got %d", mt)
	}

	// Send data.
	testData := []byte("hello relay")
	frame = make([]byte, 5+len(testData))
	binary.BigEndian.PutUint32(frame[0:4], connID)
	frame[4] = msgData
	copy(frame[5:], testData)
	ws.WriteMessage(websocket.BinaryMessage, frame)

	// Read echoed data.
	_, mt, payload := readPastControl(t, ws)
	if mt != msgData {
		t.Fatalf("expected msgData, got %d", mt)
	}
	if !bytes.Equal(payload, testData) {
		t.Fatalf("echo mismatch: got %q want %q", payload, testData)
	}

	// Send close.
	closeFrame := make([]byte, 5)
	binary.BigEndian.PutUint32(closeFrame[0:4], connID)
	closeFrame[4] = msgClose
	ws.WriteMessage(websocket.BinaryMessage, closeFrame)
}

func TestSOCKS5Handshake(t *testing.T) {
	t.Parallel()

	// Start a simple TCP server.
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("target listen: %v", err)
	}
	defer targetLn.Close()
	targetAddr := targetLn.Addr().(*net.TCPAddr)

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		conn.Write([]byte("OK"))
		conn.Close()
	}()

	wsPort := freePort(t)
	socksPort := freePort(t)

	relay, err := StartJoiner(wsPort, socksPort, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Stop()
	waitForPort(t, wsPort)
	waitForPort(t, socksPort)

	// Connect WS to joiner (simulates JS hook).
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", wsPort)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()

	// Open SOCKS5 connection.
	socksConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), 2*time.Second)
	if err != nil {
		t.Fatalf("socks dial: %v", err)
	}
	defer socksConn.Close()
	socksConn.SetDeadline(time.Now().Add(5 * time.Second))

	// SOCKS5 greeting.
	socksConn.Write([]byte{0x05, 0x01, 0x00})
	buf := make([]byte, 256)
	n, err := socksConn.Read(buf)
	if err != nil || n < 2 || buf[0] != 0x05 || buf[1] != 0x00 {
		t.Fatalf("socks greeting failed: n=%d err=%v", n, err)
	}

	// SOCKS5 CONNECT.
	ip := targetAddr.IP.To4()
	port16 := uint16(targetAddr.Port)
	req := []byte{0x05, 0x01, 0x00, 0x01, ip[0], ip[1], ip[2], ip[3], byte(port16 >> 8), byte(port16 & 0xff)}
	socksConn.Write(req)

	// Read msgConnect from WS (skip handshake/ping).
	connID, mt, pl := readPastControl(t, ws)
	if mt != msgConnect {
		t.Fatalf("expected msgConnect, got %d", mt)
	}

	// Connect to target and send msgConnectOK.
	targetConn, err := net.DialTimeout("tcp", string(pl), 2*time.Second)
	if err != nil {
		t.Fatalf("connect to target: %v", err)
	}
	defer targetConn.Close()

	okFrame := make([]byte, 5)
	binary.BigEndian.PutUint32(okFrame[0:4], connID)
	okFrame[4] = msgConnectOK
	ws.WriteMessage(websocket.BinaryMessage, okFrame)

	// Read SOCKS5 reply.
	n, err = socksConn.Read(buf)
	if err != nil || n < 2 || buf[1] != 0x00 {
		t.Fatalf("socks connect reply: n=%d rep=%d err=%v", n, buf[1], err)
	}

	// Forward target data through WS.
	go func() {
		data := make([]byte, 4096)
		n, err := targetConn.Read(data)
		if err != nil {
			return
		}
		frame := make([]byte, 5+n)
		binary.BigEndian.PutUint32(frame[0:4], connID)
		frame[4] = msgData
		copy(frame[5:], data[:n])
		ws.WriteMessage(websocket.BinaryMessage, frame)

		closeFrame := make([]byte, 5)
		binary.BigEndian.PutUint32(closeFrame[0:4], connID)
		closeFrame[4] = msgClose
		ws.WriteMessage(websocket.BinaryMessage, closeFrame)
	}()

	// Read from SOCKS.
	n, err = socksConn.Read(buf)
	if err != nil {
		t.Fatalf("socks data read: %v", err)
	}
	if !bytes.Equal(buf[:n], []byte("OK")) {
		t.Fatalf("socks data mismatch: got %q want %q", buf[:n], "OK")
	}
}

func TestMetricsTracking(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	relay, err := StartCreator(port, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Stop()

	snap := relay.GetMetrics()
	if snap.ActiveConns != 0 || snap.TotalConns != 0 {
		t.Fatalf("initial metrics should be zero: %+v", snap)
	}
}

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	relay, err := StartCreator(port, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForPort(t, port)

	relay.Stop()

	// Port should no longer be accepting connections.
	_, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		t.Fatal("port should be closed after Stop()")
	}
}

func TestHandshakeSent(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	relay, err := StartCreator(port, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Stop()
	waitForPort(t, port)

	ws, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ws", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	_, mt, payload := decodeFrame(msg)
	if mt != msgHandshake {
		t.Fatalf("first message should be handshake, got %d", mt)
	}
	if len(payload) < 1 || payload[0] != protocolVersion {
		t.Fatalf("handshake version mismatch: %v", payload)
	}
}
