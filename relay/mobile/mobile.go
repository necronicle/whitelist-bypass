package mobile

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Protocol message types.
const (
	msgConnect    byte = 0x01
	msgConnectOK  byte = 0x02
	msgConnectErr byte = 0x03
	msgData       byte = 0x04
	msgClose      byte = 0x05
	msgUDP        byte = 0x06
	msgUDPReply   byte = 0x07
	msgHandshake  byte = 0x08
	msgPing       byte = 0x09
	msgPong       byte = 0x0A
)

const (
	readBufSize     = 65536
	maxConns        = 1024
	protocolVersion = 1

	pingInterval    = 10 * time.Second
	pongTimeout     = 15 * time.Second
	connectTimeout  = 15 * time.Second
	dialTimeout     = 10 * time.Second
	udpTimeout      = 5 * time.Second

	sendQueueSize   = 4096
	backpressureMax = 1 << 20 // 1 MB
)

var framePool = sync.Pool{
	New: func() any {
		buf := make([]byte, 5+readBufSize)
		return &buf
	},
}

func encodeFrameInto(buf []byte, connID uint32, msgType byte, payload []byte) int {
	binary.BigEndian.PutUint32(buf[0:4], connID)
	buf[4] = msgType
	copy(buf[5:], payload)
	return 5 + len(payload)
}

func decodeFrame(data []byte) (connID uint32, msgType byte, payload []byte) {
	if len(data) < 5 {
		return 0, 0, nil
	}
	connID = binary.BigEndian.Uint32(data[0:4])
	msgType = data[4]
	payload = data[5:]
	return
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  readBufSize,
	WriteBufferSize: readBufSize,
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

type LogCallback interface {
	OnLog(msg string)
}

var logCb LogCallback

func slog(level, component string, connID uint32, msg string, err error) {
	var s string
	if connID > 0 && err != nil {
		s = fmt.Sprintf("level=%s component=%s conn=%d msg=%q err=%q", level, component, connID, msg, err.Error())
	} else if connID > 0 {
		s = fmt.Sprintf("level=%s component=%s conn=%d msg=%q", level, component, connID, msg)
	} else if err != nil {
		s = fmt.Sprintf("level=%s component=%s msg=%q err=%q", level, component, msg, err.Error())
	} else {
		s = fmt.Sprintf("level=%s component=%s msg=%q", level, component, msg)
	}
	if logCb != nil {
		logCb.OnLog(s)
	} else {
		log.Print(s)
	}
}

// logMsg is a short alias kept for brevity inside hot paths.
func logMsg(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if logCb != nil {
		logCb.OnLog(msg)
	} else {
		log.Print(msg)
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

// Metrics exposes relay-level counters. All fields are safe for concurrent use.
type Metrics struct {
	ActiveConns atomic.Int32
	TotalConns  atomic.Int64
	BytesSent   atomic.Int64
	BytesRecv   atomic.Int64
	Errors      atomic.Int64
}

// Snapshot returns a point-in-time copy.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		ActiveConns: int(m.ActiveConns.Load()),
		TotalConns:  m.TotalConns.Load(),
		BytesSent:   m.BytesSent.Load(),
		BytesRecv:   m.BytesRecv.Load(),
		Errors:      m.Errors.Load(),
	}
}

// MetricsSnapshot is a plain-data snapshot suitable for gomobile export.
type MetricsSnapshot struct {
	ActiveConns int
	TotalConns  int64
	BytesSent   int64
	BytesRecv   int64
	Errors      int64
}

// ---------------------------------------------------------------------------
// Relay handle (returned by Start*)
// ---------------------------------------------------------------------------

// Relay is a handle to a running relay. Call Stop() to shut it down gracefully.
type Relay struct {
	cancel  context.CancelFunc
	done    chan struct{}
	metrics Metrics
}

// Stop shuts down the relay and waits for all goroutines to finish.
func (r *Relay) Stop() {
	r.cancel()
	<-r.done
}

// GetMetrics returns a snapshot of relay metrics.
func (r *Relay) GetMetrics() MetricsSnapshot {
	return r.metrics.Snapshot()
}

// ---------------------------------------------------------------------------
// wsWriter with send queue and backpressure
// ---------------------------------------------------------------------------

type wsWriter struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	closed  bool
	pending atomic.Int64
}

func newWSWriter(ws *websocket.Conn) *wsWriter {
	return &wsWriter{ws: ws}
}

func (w *wsWriter) send(msg []byte) bool {
	cp := append([]byte(nil), msg...)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return false
	}

	w.pending.Add(int64(len(cp)))
	if err := w.ws.WriteMessage(websocket.BinaryMessage, cp); err != nil {
		w.closed = true
		logMsg("ws write error: %v", err)
		_ = w.ws.Close()
		w.pending.Add(-int64(len(cp)))
		return false
	}
	w.pending.Add(-int64(len(cp)))

	return true
}

func (w *wsWriter) backpressure() bool {
	return w.pending.Load() > backpressureMax
}

func (w *wsWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return
	}

	w.closed = true
	_ = w.ws.Close()
}

// ---------------------------------------------------------------------------
// StartJoiner / StartCreator
// ---------------------------------------------------------------------------

// StartJoiner starts the joiner relay. It returns a Relay handle immediately.
// The relay runs until Stop() is called or an unrecoverable error occurs.
func StartJoiner(wsPort, socksPort int, cb LogCallback) (*Relay, error) {
	logCb = cb
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	j := &joinerRelay{
		ready: make(chan struct{}),
	}

	relay := &Relay{cancel: cancel, done: done}
	j.metrics = &relay.metrics

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/ws", j.handleWS)

	wsServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", wsPort),
		Handler: wsMux,
	}

	socksAddr := fmt.Sprintf("127.0.0.1:%d", socksPort)
	socksLn, err := net.Listen("tcp", socksAddr)
	if err != nil {
		cancel()
		close(done)
		return nil, err
	}

	slog("INFO", "joiner", 0, fmt.Sprintf("WebSocket on 127.0.0.1:%d", wsPort), nil)
	slog("INFO", "joiner", 0, fmt.Sprintf("SOCKS5 on %s", socksAddr), nil)

	go func() {
		defer close(done)

		var wg sync.WaitGroup
		wg.Add(2)

		// WS server
		go func() {
			defer wg.Done()
			if err := wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog("ERROR", "joiner", 0, "ws server error", err)
			}
		}()

		// SOCKS listener
		go func() {
			defer wg.Done()
			j.acceptSOCKS(ctx, socksLn)
		}()

		<-ctx.Done()
		wsServer.Close()
		socksLn.Close()
		j.closeAllConns()
		wg.Wait()
	}()

	return relay, nil
}

// StartCreator starts the creator relay. It returns a Relay handle immediately.
func StartCreator(wsPort int, cb LogCallback) (*Relay, error) {
	logCb = cb
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	c := &creatorRelay{}
	relay := &Relay{cancel: cancel, done: done}
	c.metrics = &relay.metrics

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/ws", c.handleWS)

	wsServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", wsPort),
		Handler: wsMux,
	}

	slog("INFO", "creator", 0, fmt.Sprintf("WebSocket on 127.0.0.1:%d", wsPort), nil)

	go func() {
		defer close(done)
		go func() {
			if err := wsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog("ERROR", "creator", 0, "ws server error", err)
			}
		}()
		<-ctx.Done()
		wsServer.Close()
		c.closeAllConns()
	}()

	return relay, nil
}

// ---------------------------------------------------------------------------
// joinerRelay
// ---------------------------------------------------------------------------

type joinerRelay struct {
	writerMu   sync.RWMutex
	writer     *wsWriter
	conns      sync.Map
	udpClients sync.Map
	nextID     atomic.Uint32
	metrics    *Metrics
	ready      chan struct{}
	once       sync.Once
}

type socksConn struct {
	id   uint32
	conn net.Conn
	j    *joinerRelay
	rdy  chan error
}

type udpClient struct {
	udpConn     *net.UDPConn
	clientAddr  *net.UDPAddr
	socksHeader []byte
}

func (j *joinerRelay) swapWriter(next *wsWriter) {
	j.writerMu.Lock()
	prev := j.writer
	j.writer = next
	j.writerMu.Unlock()
	if prev != nil {
		prev.close()
	}
}

func (j *joinerRelay) clearWriter(target *wsWriter) {
	j.writerMu.Lock()
	if j.writer == target {
		j.writer = nil
	}
	j.writerMu.Unlock()
	if target != nil {
		target.close()
	}
}

func (j *joinerRelay) currentWriter() *wsWriter {
	j.writerMu.RLock()
	defer j.writerMu.RUnlock()
	return j.writer
}

func (j *joinerRelay) closeAllConns() {
	j.conns.Range(func(key, val any) bool {
		val.(*socksConn).conn.Close()
		j.conns.Delete(key)
		return true
	})
}

func (j *joinerRelay) handleUDPAssociate(tcpConn net.Conn) {
	udpAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		tcpConn.Close()
		return
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		tcpConn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		tcpConn.Close()
		return
	}
	localAddr := udpConn.LocalAddr().(*net.UDPAddr)
	reply := []byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0}
	binary.BigEndian.PutUint16(reply[8:10], uint16(localAddr.Port))
	tcpConn.Write(reply)
	slog("INFO", "joiner", 0, fmt.Sprintf("UDP ASSOCIATE on port %d", localAddr.Port), nil)

	go func() {
		buf := make([]byte, 1)
		tcpConn.Read(buf)
		udpConn.Close()
	}()

	go func() {
		defer udpConn.Close()
		defer tcpConn.Close()
		buf := make([]byte, readBufSize)
		var clientAddr *net.UDPAddr
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 10 {
				continue
			}
			clientAddr = addr
			frag := buf[2]
			if frag != 0 {
				continue
			}
			atyp := buf[3]
			var dstAddr string
			var headerLen int
			switch atyp {
			case 0x01:
				if n < 10 {
					continue
				}
				dstAddr = fmt.Sprintf("%d.%d.%d.%d:%d", buf[4], buf[5], buf[6], buf[7],
					binary.BigEndian.Uint16(buf[8:10]))
				headerLen = 10
			case 0x03:
				dlen := int(buf[4])
				if n < 5+dlen+2 {
					continue
				}
				dstAddr = fmt.Sprintf("%s:%d", string(buf[5:5+dlen]),
					binary.BigEndian.Uint16(buf[5+dlen:7+dlen]))
				headerLen = 5 + dlen + 2
			case 0x04:
				if n < 22 {
					continue
				}
				ip := net.IP(buf[4:20])
				dstAddr = fmt.Sprintf("[%s]:%d", ip.String(),
					binary.BigEndian.Uint16(buf[20:22]))
				headerLen = 22
			default:
				continue
			}
			id := j.nextID.Add(1)
			payload := make([]byte, len(dstAddr)+1+n-headerLen)
			payload[0] = byte(len(dstAddr))
			copy(payload[1:], dstAddr)
			copy(payload[1+len(dstAddr):], buf[headerLen:n])
			headerCopy := append([]byte(nil), buf[:headerLen]...)
			j.udpClients.Store(id, &udpClient{
				udpConn:     udpConn,
				clientAddr:  clientAddr,
				socksHeader: headerCopy,
			})
			j.send(id, msgUDP, payload)
		}
	}()
}

func (j *joinerRelay) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog("ERROR", "joiner", 0, "ws upgrade error", err)
		return
	}
	writer := newWSWriter(ws)
	j.swapWriter(writer)
	defer j.clearWriter(writer)
	j.once.Do(func() { close(j.ready) })

	// Send handshake.
	j.send(0, msgHandshake, []byte{protocolVersion})
	slog("INFO", "joiner", 0, "browser connected via WebSocket", nil)

	// Start keepalive pinger.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go j.pinger(writer, stopPing)

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			slog("WARN", "joiner", 0, "ws read error", err)
			return
		}
		connID, msgType, payload := decodeFrame(msg)
		if msgType == msgPong || msgType == msgHandshake {
			continue
		}
		if msgType == msgPing {
			j.send(0, msgPong, nil)
			continue
		}
		if len(payload) > 0 {
			j.metrics.BytesRecv.Add(int64(len(payload)))
		}
		j.handleMessage(connID, msgType, payload)
	}
}

func (j *joinerRelay) pinger(w *wsWriter, stop chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			j.send(0, msgPing, nil)
		}
	}
}

func (j *joinerRelay) handleMessage(connID uint32, msgType byte, payload []byte) {
	if msgType == msgUDPReply {
		uval, ok := j.udpClients.LoadAndDelete(connID)
		if !ok {
			return
		}
		if len(payload) == 0 {
			return
		}
		uc := uval.(*udpClient)
		reply := make([]byte, len(uc.socksHeader)+len(payload))
		copy(reply, uc.socksHeader)
		copy(reply[len(uc.socksHeader):], payload)
		uc.udpConn.WriteToUDP(reply, uc.clientAddr)
		return
	}
	val, ok := j.conns.Load(connID)
	if !ok {
		return
	}
	sc := val.(*socksConn)
	switch msgType {
	case msgConnectOK:
		sc.rdy <- nil
	case msgConnectErr:
		sc.rdy <- fmt.Errorf("%s", payload)
	case msgData:
		sc.conn.Write(payload)
	case msgClose:
		sc.conn.Close()
		j.conns.Delete(connID)
		j.metrics.ActiveConns.Add(-1)
	}
}

func (j *joinerRelay) send(connID uint32, msgType byte, payload []byte) {
	w := j.currentWriter()
	if w == nil {
		return
	}
	bufp := framePool.Get().(*[]byte)
	buf := *bufp
	n := encodeFrameInto(buf, connID, msgType, payload)
	w.send(buf[:n])
	framePool.Put(bufp)
	if len(payload) > 0 {
		j.metrics.BytesSent.Add(int64(len(payload)))
	}
}

func (j *joinerRelay) acceptSOCKS(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			slog("WARN", "joiner", 0, "accept error", err)
			continue
		}
		go j.handleSOCKS(ctx, conn)
	}
}

func (j *joinerRelay) handleSOCKS(ctx context.Context, conn net.Conn) {
	select {
	case <-j.ready:
	case <-ctx.Done():
		conn.Close()
		return
	}

	buf := make([]byte, 258)
	n, err := conn.Read(buf)
	if err != nil || n < 2 || buf[0] != 0x05 {
		conn.Close()
		return
	}
	conn.Write([]byte{0x05, 0x00})
	n, err = conn.Read(buf)
	if err != nil || n < 7 || buf[0] != 0x05 {
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}
	cmd := buf[1]
	if cmd == 0x03 {
		j.handleUDPAssociate(conn)
		return
	}
	if cmd != 0x01 {
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}
	var host string
	switch buf[3] {
	case 0x01:
		if n < 10 {
			conn.Close()
			return
		}
		host = fmt.Sprintf("%d.%d.%d.%d:%d", buf[4], buf[5], buf[6], buf[7],
			binary.BigEndian.Uint16(buf[8:10]))
	case 0x03:
		dlen := int(buf[4])
		if n < 5+dlen+2 {
			conn.Close()
			return
		}
		host = fmt.Sprintf("%s:%d", string(buf[5:5+dlen]),
			binary.BigEndian.Uint16(buf[5+dlen:7+dlen]))
	case 0x04:
		if n < 22 {
			conn.Close()
			return
		}
		ip := net.IP(buf[4:20])
		host = fmt.Sprintf("[%s]:%d", ip.String(),
			binary.BigEndian.Uint16(buf[20:22]))
	default:
		conn.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}
	if j.metrics.ActiveConns.Load() >= maxConns {
		slog("WARN", "joiner", 0, fmt.Sprintf("connection limit reached, rejecting %s", host), nil)
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}
	j.metrics.ActiveConns.Add(1)
	j.metrics.TotalConns.Add(1)
	id := j.nextID.Add(1)
	sc := &socksConn{id: id, conn: conn, j: j, rdy: make(chan error, 1)}
	j.conns.Store(id, sc)
	slog("INFO", "joiner", id, fmt.Sprintf("CONNECT -> %s", host), nil)
	j.send(id, msgConnect, []byte(host))
	select {
	case err := <-sc.rdy:
		if err != nil {
			slog("WARN", "joiner", id, "CONNECT failed", err)
			conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			conn.Close()
			j.conns.Delete(id)
			j.metrics.ActiveConns.Add(-1)
			j.metrics.Errors.Add(1)
			return
		}
	case <-time.After(connectTimeout):
		slog("WARN", "joiner", id, "CONNECT timed out", nil)
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		j.conns.Delete(id)
		j.metrics.ActiveConns.Add(-1)
		j.metrics.Errors.Add(1)
		return
	case <-ctx.Done():
		conn.Close()
		j.conns.Delete(id)
		j.metrics.ActiveConns.Add(-1)
		return
	}
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	slog("INFO", "joiner", id, fmt.Sprintf("CONNECTED -> %s", host), nil)
	go func() {
		defer j.metrics.ActiveConns.Add(-1)
		buf := make([]byte, readBufSize)
		for {
			// Backpressure: pause reading if WS buffer is full.
			w := j.currentWriter()
			if w != nil && w.backpressure() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			n, err := conn.Read(buf)
			if n > 0 {
				j.send(id, msgData, buf[:n])
			}
			if err != nil {
				j.send(id, msgClose, nil)
				j.conns.Delete(id)
				return
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// creatorRelay
// ---------------------------------------------------------------------------

type creatorRelay struct {
	writerMu sync.RWMutex
	writer   *wsWriter
	conns    sync.Map
	metrics  *Metrics
}

func (c *creatorRelay) swapWriter(next *wsWriter) {
	c.writerMu.Lock()
	prev := c.writer
	c.writer = next
	c.writerMu.Unlock()
	if prev != nil {
		prev.close()
	}
}

func (c *creatorRelay) clearWriter(target *wsWriter) {
	c.writerMu.Lock()
	if c.writer == target {
		c.writer = nil
	}
	c.writerMu.Unlock()
	if target != nil {
		target.close()
	}
}

func (c *creatorRelay) currentWriter() *wsWriter {
	c.writerMu.RLock()
	defer c.writerMu.RUnlock()
	return c.writer
}

func (c *creatorRelay) closeAllConns() {
	c.conns.Range(func(key, val any) bool {
		val.(net.Conn).Close()
		c.conns.Delete(key)
		return true
	})
}

func (c *creatorRelay) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog("ERROR", "creator", 0, "ws upgrade error", err)
		return
	}
	writer := newWSWriter(ws)
	c.swapWriter(writer)
	defer c.clearWriter(writer)

	// Send handshake.
	c.send(0, msgHandshake, []byte{protocolVersion})
	slog("INFO", "creator", 0, "browser connected via WebSocket", nil)

	// Start keepalive pinger.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go c.pinger(writer, stopPing)

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			slog("WARN", "creator", 0, "ws read error", err)
			return
		}
		connID, msgType, payload := decodeFrame(msg)
		if msgType == msgPong || msgType == msgHandshake {
			continue
		}
		if msgType == msgPing {
			c.send(0, msgPong, nil)
			continue
		}
		if len(payload) > 0 {
			c.metrics.BytesRecv.Add(int64(len(payload)))
		}
		c.handleMessage(connID, msgType, payload)
	}
}

func (c *creatorRelay) pinger(w *wsWriter, stop chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.send(0, msgPing, nil)
		}
	}
}

func (c *creatorRelay) handleMessage(connID uint32, msgType byte, payload []byte) {
	switch msgType {
	case msgConnect:
		go c.connect(connID, string(payload))
	case msgUDP:
		go c.handleUDP(connID, payload)
	case msgData:
		val, ok := c.conns.Load(connID)
		if ok {
			val.(net.Conn).Write(payload)
		}
	case msgClose:
		val, ok := c.conns.LoadAndDelete(connID)
		if ok {
			val.(net.Conn).Close()
		}
	}
}

func (c *creatorRelay) send(connID uint32, msgType byte, payload []byte) {
	w := c.currentWriter()
	if w == nil {
		return
	}
	bufp := framePool.Get().(*[]byte)
	buf := *bufp
	n := encodeFrameInto(buf, connID, msgType, payload)
	w.send(buf[:n])
	framePool.Put(bufp)
	if len(payload) > 0 {
		c.metrics.BytesSent.Add(int64(len(payload)))
	}
}

func (c *creatorRelay) handleUDP(connID uint32, payload []byte) {
	if len(payload) < 2 {
		return
	}
	addrLen := int(payload[0])
	if len(payload) < 1+addrLen {
		return
	}
	addr := string(payload[1 : 1+addrLen])
	data := payload[1+addrLen:]

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		slog("WARN", "creator", connID, fmt.Sprintf("UDP resolve %s failed", addr), err)
		c.send(connID, msgUDPReply, nil)
		c.metrics.Errors.Add(1)
		return
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		slog("WARN", "creator", connID, fmt.Sprintf("UDP dial %s failed", addr), err)
		c.send(connID, msgUDPReply, nil)
		c.metrics.Errors.Add(1)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(udpTimeout))
	_, err = conn.Write(data)
	if err != nil {
		slog("WARN", "creator", connID, fmt.Sprintf("UDP write %s failed", addr), err)
		c.send(connID, msgUDPReply, nil)
		c.metrics.Errors.Add(1)
		return
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		slog("WARN", "creator", connID, fmt.Sprintf("UDP read %s failed", addr), err)
		c.send(connID, msgUDPReply, nil)
		c.metrics.Errors.Add(1)
		return
	}
	c.send(connID, msgUDPReply, buf[:n])
}

func (c *creatorRelay) connect(connID uint32, addr string) {
	if c.metrics.ActiveConns.Load() >= maxConns {
		slog("WARN", "creator", connID, fmt.Sprintf("connection limit reached, rejecting %s", addr), nil)
		c.send(connID, msgConnectErr, []byte("connection limit reached"))
		return
	}
	c.metrics.ActiveConns.Add(1)
	c.metrics.TotalConns.Add(1)
	slog("INFO", "creator", connID, fmt.Sprintf("CONNECT -> %s", addr), nil)
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		slog("WARN", "creator", connID, "CONNECT failed", err)
		c.send(connID, msgConnectErr, []byte(err.Error()))
		c.metrics.ActiveConns.Add(-1)
		c.metrics.Errors.Add(1)
		return
	}
	c.conns.Store(connID, conn)
	c.send(connID, msgConnectOK, nil)
	slog("INFO", "creator", connID, fmt.Sprintf("CONNECTED -> %s", addr), nil)
	defer c.metrics.ActiveConns.Add(-1)
	buf := make([]byte, readBufSize)
	for {
		// Backpressure: pause reading if WS buffer is full.
		w := c.currentWriter()
		if w != nil && w.backpressure() {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		n, err := conn.Read(buf)
		if n > 0 {
			c.send(connID, msgData, buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				slog("WARN", "creator", connID, "read error", err)
			}
			break
		}
	}
	c.send(connID, msgClose, nil)
	c.conns.Delete(connID)
}
