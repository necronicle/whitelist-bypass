package pion

import (
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/pion/webrtc/v4"
)

// VP8 data encoding format:
//
//	[0xFF marker][4-byte big-endian length][payload]
//
// The 0xFF marker byte cannot appear as the first byte of a real VP8 frame:
//   - VP8 keyframes start with a 3-byte frame tag where bit0=0 (keyframe).
//   - VP8 interframes have bit0=1, but 0xFF would require specific bit patterns
//     that don't occur in practice for the partition/version fields.
//
// This allows the receiver to distinguish data frames from keepalive VP8 frames.
const (
	vp8DataMarker    byte = 0xFF
	vp8DataHeaderLen      = 5 // 1 marker + 4 length

	vp8SendQueueCap  = 256
	vp8MinSendInterval = 5 * time.Millisecond
	vp8KeyframeEvery = 60
	vp8FrameDuration = 40 * time.Millisecond // 25fps
)

// Minimal valid VP8 frames for keepalive.
// These are the smallest valid VP8 bitstreams that decoders will accept.
var (
	// VP8 keyframe: 10-byte minimal frame.
	// Frame tag (3 bytes): 0x10 0x01 0x00 -> keyframe, version 0, show_frame=1, partition_size=1
	// Then 7 bytes: start code (0x9D 0x01 0x2A) + width=2 (0x02 0x00) + height=2 (0x00 0x02)
	vp8Keyframe = []byte{0x10, 0x01, 0x00, 0x9D, 0x01, 0x2A, 0x02, 0x00, 0x02, 0x00}

	// VP8 interframe: 3-byte minimal frame.
	// Frame tag: 0x11 0x00 0x00 -> interframe, version 0, show_frame=1, partition_size=0
	vp8Interframe = []byte{0x11, 0x00, 0x00}
)

// VP8DataTunnel encodes arbitrary data inside VP8 video samples and sends
// them via a Pion static sample track. Keepalive VP8 frames maintain the
// video stream when no data is pending.
type VP8DataTunnel struct {
	track  *webrtc.TrackLocalStaticSample
	dataCh chan []byte
	stopCh chan struct{}
	once   sync.Once

	frameCount atomic.Uint64
	dataBytes  atomic.Int64
}

// NewVP8DataTunnel creates a tunnel bound to the given VP8 video track.
func NewVP8DataTunnel(track *webrtc.TrackLocalStaticSample) *VP8DataTunnel {
	return &VP8DataTunnel{
		track:  track,
		dataCh: make(chan []byte, vp8SendQueueCap),
		stopCh: make(chan struct{}),
	}
}

// Start launches the background sender goroutine. It returns immediately.
func (t *VP8DataTunnel) Start() {
	go t.sendLoop()
}

// Stop shuts down the sender goroutine.
func (t *VP8DataTunnel) Stop() {
	t.once.Do(func() { close(t.stopCh) })
}

// SendData queues data for transmission inside a VP8 frame.
// Returns an error if the send queue is full.
func (t *VP8DataTunnel) SendData(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case t.dataCh <- cp:
		return nil
	default:
		return fmt.Errorf("vp8 send queue full (cap %d)", vp8SendQueueCap)
	}
}

// EncodeDataPayload wraps data in the VP8 data frame format: [0xFF][4B len][payload].
func EncodeDataPayload(data []byte) []byte {
	out := make([]byte, vp8DataHeaderLen+len(data))
	out[0] = vp8DataMarker
	binary.BigEndian.PutUint32(out[1:5], uint32(len(data)))
	copy(out[5:], data)
	return out
}

// ExtractDataFromPayload checks if payload is a VP8 data frame (starts with 0xFF).
// Returns the embedded data and true, or nil and false for real VP8 frames.
func ExtractDataFromPayload(payload []byte) ([]byte, bool) {
	if len(payload) < vp8DataHeaderLen {
		return nil, false
	}
	if payload[0] != vp8DataMarker {
		return nil, false
	}
	dataLen := binary.BigEndian.Uint32(payload[1:5])
	if int(dataLen) > len(payload)-vp8DataHeaderLen {
		return nil, false
	}
	return payload[vp8DataHeaderLen : vp8DataHeaderLen+int(dataLen)], true
}

func (t *VP8DataTunnel) sendLoop() {
	ticker := time.NewTicker(vp8FrameDuration)
	defer ticker.Stop()

	var lastSend time.Time
	var count uint64

	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			// Try to drain queued data first.
			drained := false
			for {
				select {
				case data := <-t.dataCh:
					now := time.Now()
					if elapsed := now.Sub(lastSend); elapsed < vp8MinSendInterval {
						time.Sleep(vp8MinSendInterval - elapsed)
					}
					payload := EncodeDataPayload(data)
					t.writeSample(payload)
					lastSend = time.Now()
					t.dataBytes.Add(int64(len(data)))
					drained = true
				default:
					goto done
				}
			}
		done:
			if drained {
				count++
				t.frameCount.Store(count)
				continue
			}
			// No data queued — send keepalive VP8 frame.
			count++
			t.frameCount.Store(count)
			if count%vp8KeyframeEvery == 0 {
				t.writeSample(vp8Keyframe)
			} else {
				t.writeSample(vp8Interframe)
			}
		}
	}
}

func (t *VP8DataTunnel) writeSample(payload []byte) {
	_ = t.track.WriteSample(media.Sample{
		Data:     payload,
		Duration: vp8FrameDuration,
	})
}

// Stats returns tunnel statistics.
func (t *VP8DataTunnel) Stats() (frames uint64, dataBytes int64) {
	return t.frameCount.Load(), t.dataBytes.Load()
}
