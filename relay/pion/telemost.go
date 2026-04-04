package pion

import (
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// TelemostClient handles Yandex Telemost signaling and creates a Pion
// PeerConnection with a VP8 video track for data tunneling.
//
// Telemost uses a different signaling protocol (strm.yandex WebSocket)
// with publisherSdpOffer/publisherSdpAnswer message formats, but the
// WebRTC layer is identical to VK.
type TelemostClient struct {
	pc     *webrtc.PeerConnection
	tunnel *VP8DataTunnel
	bridge *RelayBridge
	logFn  LogFunc
	sigSrv *SignalingServer

	mu                sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
	remoteDescSet     bool
}

// NewTelemostClient creates a Telemost client that uses Pion for WebRTC.
func NewTelemostClient(bridge *RelayBridge, logFn LogFunc) *TelemostClient {
	if logFn == nil {
		logFn = func(msg string) {}
	}
	c := &TelemostClient{
		bridge: bridge,
		logFn:  logFn,
	}
	c.sigSrv = NewSignalingServer(c, logFn)
	return c
}

// SignalingServer returns the signaling server for this client.
func (c *TelemostClient) SignalingServer() *SignalingServer {
	return c.sigSrv
}

// OnICEServers is called when ICE server config arrives from the browser hook.
func (c *TelemostClient) OnICEServers(servers []webrtc.ICEServer) error {
	c.logFn(fmt.Sprintf("[telemost] received %d ICE servers", len(servers)))

	config := webrtc.Configuration{
		ICEServers:         servers,
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create PeerConnection: %w", err)
	}

	// Add VP8 video track.
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"tunnel-video",
		"tunnel-stream",
	)
	if err != nil {
		pc.Close()
		return fmt.Errorf("create track: %w", err)
	}

	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return fmt.Errorf("add track: %w", err)
	}

	// Also add an audio track (Telemost expects both audio and video).
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"tunnel-audio",
		"tunnel-stream",
	)
	if err != nil {
		pc.Close()
		return fmt.Errorf("create audio track: %w", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		pc.Close()
		return fmt.Errorf("add audio track: %w", err)
	}

	tunnel := NewVP8DataTunnel(track)
	tunnel.Start()

	c.mu.Lock()
	c.pc = pc
	c.tunnel = tunnel
	c.bridge.tunnel = tunnel
	c.mu.Unlock()

	pc.OnTrack(func(remoteTrack *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		if remoteTrack.Codec().MimeType != webrtc.MimeTypeVP8 {
			return
		}
		c.logFn("[telemost] incoming VP8 track, starting data extraction")
		go c.readTrack(remoteTrack)
	})

	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		init := candidate.ToJSON()
		c.sigSrv.SendToHook(SignalingMessage{
			Type: "candidate",
			Candidate: &ICECandidateMsg{
				Candidate:     init.Candidate,
				SDPMid:        init.SDPMid,
				SDPMLineIndex: init.SDPMLineIndex,
			},
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		c.logFn(fmt.Sprintf("[telemost] connection state: %s", state.String()))
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			tunnel.Stop()
		}
	})

	return nil
}

// OnOffer handles an SDP offer from the browser hook.
func (c *TelemostClient) OnOffer(sdp string) (*webrtc.SessionDescription, error) {
	c.mu.Lock()
	pc := c.pc
	c.mu.Unlock()

	if pc == nil {
		return nil, fmt.Errorf("PeerConnection not initialized")
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	c.mu.Lock()
	c.remoteDescSet = true
	pending := c.pendingCandidates
	c.pendingCandidates = nil
	c.mu.Unlock()

	for _, candidate := range pending {
		pc.AddICECandidate(candidate)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}

	c.logFn("[telemost] SDP offer/answer exchange complete")
	return &answer, nil
}

// OnAnswer handles an SDP answer from the browser hook.
func (c *TelemostClient) OnAnswer(sdp string) error {
	c.mu.Lock()
	pc := c.pc
	c.mu.Unlock()

	if pc == nil {
		return fmt.Errorf("PeerConnection not initialized")
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	c.mu.Lock()
	c.remoteDescSet = true
	pending := c.pendingCandidates
	c.pendingCandidates = nil
	c.mu.Unlock()

	for _, candidate := range pending {
		pc.AddICECandidate(candidate)
	}

	c.logFn("[telemost] SDP answer set")
	return nil
}

// OnCandidate handles an ICE candidate from the browser hook.
func (c *TelemostClient) OnCandidate(candidate ICECandidateMsg) error {
	init := webrtc.ICECandidateInit{
		Candidate:     candidate.Candidate,
		SDPMid:        candidate.SDPMid,
		SDPMLineIndex: candidate.SDPMLineIndex,
	}

	c.mu.Lock()
	if !c.remoteDescSet {
		c.pendingCandidates = append(c.pendingCandidates, init)
		c.mu.Unlock()
		return nil
	}
	pc := c.pc
	c.mu.Unlock()

	if pc == nil {
		return fmt.Errorf("PeerConnection not initialized")
	}
	return pc.AddICECandidate(init)
}

func (c *TelemostClient) readTrack(track *webrtc.TrackRemote) {
	buf := make([]byte, 1500)
	for {
		n, _, err := track.Read(buf)
		if err != nil {
			c.logFn(fmt.Sprintf("[telemost] track read error: %v", err))
			return
		}
		data, ok := ExtractDataFromPayload(buf[:n])
		if ok && len(data) > 0 {
			c.bridge.HandleIncoming(data)
		}
	}
}

// Close shuts down the Telemost client.
func (c *TelemostClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.tunnel != nil {
		c.tunnel.Stop()
	}
	if c.pc != nil {
		c.pc.Close()
	}
}
