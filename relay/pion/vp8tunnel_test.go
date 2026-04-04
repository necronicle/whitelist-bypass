package pion

import (
	"bytes"
	"testing"
)

func TestEncodeExtractRoundTrip(t *testing.T) {
	t.Parallel()

	data := []byte("hello tunnel data")
	encoded := EncodeDataPayload(data)

	if encoded[0] != vp8DataMarker {
		t.Fatalf("expected marker 0xFF, got 0x%02X", encoded[0])
	}

	extracted, ok := ExtractDataFromPayload(encoded)
	if !ok {
		t.Fatal("ExtractDataFromPayload returned false for valid data frame")
	}
	if !bytes.Equal(extracted, data) {
		t.Fatalf("roundtrip mismatch: got %q want %q", extracted, data)
	}
}

func TestExtractRejectsRealVP8Keyframe(t *testing.T) {
	t.Parallel()

	_, ok := ExtractDataFromPayload(vp8Keyframe)
	if ok {
		t.Fatal("should not extract data from real VP8 keyframe")
	}
}

func TestExtractRejectsRealVP8Interframe(t *testing.T) {
	t.Parallel()

	_, ok := ExtractDataFromPayload(vp8Interframe)
	if ok {
		t.Fatal("should not extract data from real VP8 interframe")
	}
}

func TestExtractRejectsShortPayload(t *testing.T) {
	t.Parallel()

	_, ok := ExtractDataFromPayload([]byte{0xFF, 0x00})
	if ok {
		t.Fatal("should reject payload shorter than header")
	}
}

func TestExtractRejectsTruncatedPayload(t *testing.T) {
	t.Parallel()

	// Marker + length=100 but only 2 bytes of actual data.
	payload := []byte{0xFF, 0x00, 0x00, 0x00, 0x64, 0xAA, 0xBB}
	_, ok := ExtractDataFromPayload(payload)
	if ok {
		t.Fatal("should reject truncated payload")
	}
}

func TestEncodeEmptyData(t *testing.T) {
	t.Parallel()

	encoded := EncodeDataPayload([]byte{})
	extracted, ok := ExtractDataFromPayload(encoded)
	if !ok {
		t.Fatal("should extract empty data")
	}
	if len(extracted) != 0 {
		t.Fatalf("expected empty, got %d bytes", len(extracted))
	}
}

func TestEncodeLargeData(t *testing.T) {
	t.Parallel()

	data := make([]byte, 65536)
	for i := range data {
		data[i] = byte(i % 256)
	}

	encoded := EncodeDataPayload(data)
	extracted, ok := ExtractDataFromPayload(encoded)
	if !ok {
		t.Fatal("should extract large data")
	}
	if !bytes.Equal(extracted, data) {
		t.Fatal("large data roundtrip mismatch")
	}
}
