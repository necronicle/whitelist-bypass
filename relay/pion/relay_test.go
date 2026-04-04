package pion

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRelayFrameRoundTrip(t *testing.T) {
	t.Parallel()

	payload := []byte("test payload data")
	frame := EncodeRelayFrame(42, MsgData, payload)

	connID, msgType, decoded, n := DecodeRelayFrame(frame)
	if n != len(frame) {
		t.Fatalf("consumed %d bytes, want %d", n, len(frame))
	}
	if connID != 42 {
		t.Fatalf("connID: got %d want 42", connID)
	}
	if msgType != MsgData {
		t.Fatalf("msgType: got 0x%02X want 0x%02X", msgType, MsgData)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("payload mismatch: got %q want %q", decoded, payload)
	}
}

func TestDecodeRelayFrameRejectsShort(t *testing.T) {
	t.Parallel()

	_, _, _, n := DecodeRelayFrame([]byte{1, 2, 3})
	if n != 0 {
		t.Fatal("should reject short frame")
	}
}

func TestDecodeRelayFrameRejectsTruncated(t *testing.T) {
	t.Parallel()

	// Length says 100 but only 6 bytes available.
	frame := []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x00}
	_, _, _, n := DecodeRelayFrame(frame)
	if n != 0 {
		t.Fatal("should reject truncated frame")
	}
}

func TestMultipleFramesParsing(t *testing.T) {
	t.Parallel()

	frame1 := EncodeRelayFrame(1, MsgConnect, []byte("host:80"))
	frame2 := EncodeRelayFrame(2, MsgData, []byte("hello"))
	combined := append(frame1, frame2...)

	connID1, mt1, pl1, n1 := DecodeRelayFrame(combined)
	if n1 == 0 {
		t.Fatal("failed to decode first frame")
	}
	if connID1 != 1 || mt1 != MsgConnect || string(pl1) != "host:80" {
		t.Fatalf("frame1 mismatch: conn=%d type=0x%02X payload=%q", connID1, mt1, pl1)
	}

	connID2, mt2, pl2, n2 := DecodeRelayFrame(combined[n1:])
	if n2 == 0 {
		t.Fatal("failed to decode second frame")
	}
	if connID2 != 2 || mt2 != MsgData || string(pl2) != "hello" {
		t.Fatalf("frame2 mismatch: conn=%d type=0x%02X payload=%q", connID2, mt2, pl2)
	}
}

func TestEncodeRelayFrameEmptyPayload(t *testing.T) {
	t.Parallel()

	frame := EncodeRelayFrame(0, MsgPing, nil)
	connID, msgType, payload, n := DecodeRelayFrame(frame)
	if n == 0 {
		t.Fatal("failed to decode frame with empty payload")
	}
	if connID != 0 || msgType != MsgPing || len(payload) != 0 {
		t.Fatalf("unexpected: conn=%d type=0x%02X payload=%q", connID, msgType, payload)
	}
}
