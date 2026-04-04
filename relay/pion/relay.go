package pion

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Protocol message types — identical to mobile package values.
const (
	MsgConnect    byte = 0x01
	MsgConnectOK  byte = 0x02
	MsgConnectErr byte = 0x03
	MsgData       byte = 0x04
	MsgClose      byte = 0x05
	MsgUDP        byte = 0x06
	MsgUDPReply   byte = 0x07
	MsgHandshake  byte = 0x08
	MsgPing       byte = 0x09
	MsgPong       byte = 0x0A
)

// Relay frame format (length-prefixed for VP8 transport):
//
//	[4B frame length][4B connID][1B msgType][payload]
//
// The 4-byte length prefix covers connID + msgType + payload.
const (
	relayFrameHeaderLen = 9 // 4 length + 4 connID + 1 msgType
	relayReadBufSize    = 65536
	relayMaxConns       = 1024
	relayDialTimeout    = 10 * time.Second
	relayConnectTimeout = 15 * time.Second
	relayUDPTimeout     = 5 * time.Second
)

// RelayMetrics exposes relay-level counters.
type RelayMetrics struct {
	ActiveConns atomic.Int32
	TotalConns  atomic.Int64
	BytesSent   atomic.Int64
	BytesRecv   atomic.Int64
	Errors      atomic.Int64
}

// LogFunc is the logging callback type.
type LogFunc func(msg string)

// RelayBridge multiplexes TCP/UDP connections over a VP8DataTunnel.
type RelayBridge struct {
	tunnel  *VP8DataTunnel
	conns   sync.Map // connID -> net.Conn (creator) or *bridgeConn (joiner)
	nextID  atomic.Uint32
	mode    string // "joiner" or "creator"
	metrics RelayMetrics
	logFn   LogFunc
	ctx     context.Context
	cancel  context.CancelFunc
}

type bridgeConn struct {
	id   uint32
	conn net.Conn
	rdy  chan error
}

// NewRelayBridge creates a relay bridge over the given VP8 tunnel.
func NewRelayBridge(tunnel *VP8DataTunnel, mode string, logFn LogFunc) *RelayBridge {
	ctx, cancel := context.WithCancel(context.Background())
	if logFn == nil {
		logFn = func(msg string) { log.Print(msg) }
	}
	return &RelayBridge{
		tunnel: tunnel,
		mode:   mode,
		logFn:  logFn,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Stop shuts down the relay bridge.
func (b *RelayBridge) Stop() {
	b.cancel()
	b.conns.Range(func(key, val any) bool {
		switch c := val.(type) {
		case net.Conn:
			c.Close()
		case *bridgeConn:
			c.conn.Close()
		}
		b.conns.Delete(key)
		return true
	})
}

// Metrics returns current relay metrics.
func (b *RelayBridge) Metrics() RelayMetrics {
	return b.metrics
}

// EncodeRelayFrame builds a length-prefixed relay frame.
func EncodeRelayFrame(connID uint32, msgType byte, payload []byte) []byte {
	innerLen := 4 + 1 + len(payload) // connID + msgType + payload
	frame := make([]byte, 4+innerLen)
	binary.BigEndian.PutUint32(frame[0:4], uint32(innerLen))
	binary.BigEndian.PutUint32(frame[4:8], connID)
	frame[8] = msgType
	copy(frame[9:], payload)
	return frame
}

// DecodeRelayFrame parses a length-prefixed relay frame.
// Returns connID, msgType, payload, bytesConsumed.
func DecodeRelayFrame(data []byte) (connID uint32, msgType byte, payload []byte, n int) {
	if len(data) < relayFrameHeaderLen {
		return 0, 0, nil, 0
	}
	innerLen := int(binary.BigEndian.Uint32(data[0:4]))
	totalLen := 4 + innerLen
	if len(data) < totalLen || innerLen < 5 {
		return 0, 0, nil, 0
	}
	connID = binary.BigEndian.Uint32(data[4:8])
	msgType = data[8]
	payload = data[9:totalLen]
	return connID, msgType, payload, totalLen
}

// Send sends a relay frame through the VP8 tunnel.
func (b *RelayBridge) Send(connID uint32, msgType byte, payload []byte) {
	frame := EncodeRelayFrame(connID, msgType, payload)
	if err := b.tunnel.SendData(frame); err != nil {
		b.logFn(fmt.Sprintf("[relay] send queue full conn=%d type=0x%02X", connID, msgType))
	}
	if len(payload) > 0 {
		b.metrics.BytesSent.Add(int64(len(payload)))
	}
}

// HandleIncoming processes data extracted from incoming VP8 frames.
// The data may contain multiple concatenated relay frames.
func (b *RelayBridge) HandleIncoming(data []byte) {
	for len(data) > 0 {
		connID, msgType, payload, n := DecodeRelayFrame(data)
		if n == 0 {
			break
		}
		data = data[n:]

		if len(payload) > 0 {
			b.metrics.BytesRecv.Add(int64(len(payload)))
		}

		if msgType == MsgPing {
			b.Send(0, MsgPong, nil)
			continue
		}
		if msgType == MsgPong || msgType == MsgHandshake {
			continue
		}

		switch b.mode {
		case "creator":
			b.handleCreatorMessage(connID, msgType, payload)
		case "joiner":
			b.handleJoinerMessage(connID, msgType, payload)
		}
	}
}

// ---------------------------------------------------------------------------
// Creator mode: receives CONNECT requests, dials outbound
// ---------------------------------------------------------------------------

func (b *RelayBridge) handleCreatorMessage(connID uint32, msgType byte, payload []byte) {
	switch msgType {
	case MsgConnect:
		go b.creatorConnect(connID, string(payload))
	case MsgUDP:
		go b.creatorHandleUDP(connID, payload)
	case MsgData:
		val, ok := b.conns.Load(connID)
		if ok {
			val.(net.Conn).Write(payload)
		}
	case MsgClose:
		val, ok := b.conns.LoadAndDelete(connID)
		if ok {
			val.(net.Conn).Close()
		}
	}
}

func (b *RelayBridge) creatorConnect(connID uint32, addr string) {
	if b.metrics.ActiveConns.Load() >= relayMaxConns {
		b.logFn(fmt.Sprintf("[relay] connection limit, rejecting %s", maskAddr(addr)))
		b.Send(connID, MsgConnectErr, []byte("connection limit reached"))
		return
	}
	b.metrics.ActiveConns.Add(1)
	b.metrics.TotalConns.Add(1)
	b.logFn(fmt.Sprintf("[relay] CONNECT %d -> %s", connID, maskAddr(addr)))

	conn, err := net.DialTimeout("tcp", addr, relayDialTimeout)
	if err != nil {
		b.logFn(fmt.Sprintf("[relay] CONNECT %d failed: %v", connID, err))
		b.Send(connID, MsgConnectErr, []byte(err.Error()))
		b.metrics.ActiveConns.Add(-1)
		b.metrics.Errors.Add(1)
		return
	}
	b.conns.Store(connID, conn)
	b.Send(connID, MsgConnectOK, nil)
	b.logFn(fmt.Sprintf("[relay] CONNECTED %d -> %s", connID, maskAddr(addr)))

	defer b.metrics.ActiveConns.Add(-1)
	buf := make([]byte, relayReadBufSize)
	for {
		select {
		case <-b.ctx.Done():
			return
		default:
		}
		n, err := conn.Read(buf)
		if n > 0 {
			b.Send(connID, MsgData, buf[:n])
		}
		if err != nil {
			if err != io.EOF {
				b.logFn(fmt.Sprintf("[relay] read %d error: %v", connID, err))
			}
			break
		}
	}
	b.Send(connID, MsgClose, nil)
	b.conns.Delete(connID)
}

func (b *RelayBridge) creatorHandleUDP(connID uint32, payload []byte) {
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
		b.logFn(fmt.Sprintf("[relay] UDP resolve %s failed: %v", maskAddr(addr), err))
		b.Send(connID, MsgUDPReply, nil)
		b.metrics.Errors.Add(1)
		return
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		b.logFn(fmt.Sprintf("[relay] UDP dial %s failed: %v", maskAddr(addr), err))
		b.Send(connID, MsgUDPReply, nil)
		b.metrics.Errors.Add(1)
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(relayUDPTimeout))
	_, err = conn.Write(data)
	if err != nil {
		b.Send(connID, MsgUDPReply, nil)
		b.metrics.Errors.Add(1)
		return
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		b.Send(connID, MsgUDPReply, nil)
		b.metrics.Errors.Add(1)
		return
	}
	b.Send(connID, MsgUDPReply, buf[:n])
}

// ---------------------------------------------------------------------------
// Joiner mode: runs SOCKS5, routes through tunnel
// ---------------------------------------------------------------------------

func (b *RelayBridge) handleJoinerMessage(connID uint32, msgType byte, payload []byte) {
	val, ok := b.conns.Load(connID)
	if !ok {
		return
	}
	bc := val.(*bridgeConn)
	switch msgType {
	case MsgConnectOK:
		bc.rdy <- nil
	case MsgConnectErr:
		bc.rdy <- fmt.Errorf("%s", payload)
	case MsgData:
		bc.conn.Write(payload)
	case MsgClose:
		bc.conn.Close()
		b.conns.Delete(connID)
		b.metrics.ActiveConns.Add(-1)
	}
}

// StartSOCKS5 starts accepting SOCKS5 connections on the given port.
// Blocks until the context is cancelled.
func (b *RelayBridge) StartSOCKS5(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.logFn(fmt.Sprintf("[relay] SOCKS5 listening on %s", addr))

	go func() {
		<-b.ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-b.ctx.Done():
				return nil
			default:
			}
			continue
		}
		go b.handleSOCKS5(conn)
	}
}

func (b *RelayBridge) handleSOCKS5(conn net.Conn) {
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
	if cmd != 0x01 {
		conn.Write([]byte{0x05, 0x07, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}

	var host string
	switch buf[3] {
	case 0x01: // IPv4
		if n < 10 {
			conn.Close()
			return
		}
		host = fmt.Sprintf("%d.%d.%d.%d:%d", buf[4], buf[5], buf[6], buf[7],
			binary.BigEndian.Uint16(buf[8:10]))
	case 0x03: // Domain
		dlen := int(buf[4])
		if n < 5+dlen+2 {
			conn.Close()
			return
		}
		host = fmt.Sprintf("%s:%d", string(buf[5:5+dlen]),
			binary.BigEndian.Uint16(buf[5+dlen:7+dlen]))
	case 0x04: // IPv6
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

	if b.metrics.ActiveConns.Load() >= relayMaxConns {
		b.logFn(fmt.Sprintf("[relay] connection limit, rejecting %s", maskAddr(host)))
		conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		return
	}

	b.metrics.ActiveConns.Add(1)
	b.metrics.TotalConns.Add(1)
	id := b.nextID.Add(1)
	bc := &bridgeConn{id: id, conn: conn, rdy: make(chan error, 1)}
	b.conns.Store(id, bc)

	b.logFn(fmt.Sprintf("[relay] CONNECT %d -> %s", id, maskAddr(host)))
	b.Send(id, MsgConnect, []byte(host))

	select {
	case err := <-bc.rdy:
		if err != nil {
			b.logFn(fmt.Sprintf("[relay] CONNECT %d failed: %v", id, err))
			conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			conn.Close()
			b.conns.Delete(id)
			b.metrics.ActiveConns.Add(-1)
			b.metrics.Errors.Add(1)
			return
		}
	case <-time.After(relayConnectTimeout):
		b.logFn(fmt.Sprintf("[relay] CONNECT %d timed out", id))
		conn.Write([]byte{0x05, 0x04, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		conn.Close()
		b.conns.Delete(id)
		b.metrics.ActiveConns.Add(-1)
		b.metrics.Errors.Add(1)
		return
	case <-b.ctx.Done():
		conn.Close()
		b.conns.Delete(id)
		b.metrics.ActiveConns.Add(-1)
		return
	}

	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	b.logFn(fmt.Sprintf("[relay] CONNECTED %d -> %s", id, maskAddr(host)))

	go func() {
		defer b.metrics.ActiveConns.Add(-1)
		readBuf := make([]byte, relayReadBufSize)
		for {
			rn, rerr := conn.Read(readBuf)
			if rn > 0 {
				b.Send(id, MsgData, readBuf[:rn])
			}
			if rerr != nil {
				b.Send(id, MsgClose, nil)
				b.conns.Delete(id)
				return
			}
		}
	}()
}
