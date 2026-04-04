package pion

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var sigUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  65536,
	WriteBufferSize: 65536,
}

// SignalingMessage is the JSON envelope exchanged over the signaling WebSocket.
type SignalingMessage struct {
	Type      string           `json:"type"`
	SDP       string           `json:"sdp,omitempty"`
	SDPType   string           `json:"sdpType,omitempty"`
	Candidate *ICECandidateMsg `json:"candidate,omitempty"`
	Servers   []ICEServerJSON  `json:"servers,omitempty"`
	Role      string           `json:"role,omitempty"`
}

// ICECandidateMsg mirrors the JS RTCIceCandidate.
type ICECandidateMsg struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// ICEServerJSON is the JSON format for ICE server config from the browser.
type ICEServerJSON struct {
	URLs       interface{} `json:"urls"` // string or []string
	Username   string      `json:"username,omitempty"`
	Credential string      `json:"credential,omitempty"`
}

// ParseICEServers converts JSON ICE server configs to Pion format.
func ParseICEServers(servers []ICEServerJSON) []webrtc.ICEServer {
	var result []webrtc.ICEServer
	for _, s := range servers {
		is := webrtc.ICEServer{
			Username:   s.Username,
			Credential: s.Credential,
		}
		switch v := s.URLs.(type) {
		case string:
			is.URLs = []string{v}
		case []interface{}:
			for _, u := range v {
				if us, ok := u.(string); ok {
					is.URLs = append(is.URLs, us)
				}
			}
		}
		result = append(result, is)
	}
	return result
}

// SignalingHandler is the interface that platform-specific clients implement
// to handle signaling messages from the browser hook.
type SignalingHandler interface {
	OnOffer(sdp string) (*webrtc.SessionDescription, error)
	OnAnswer(sdp string) error
	OnCandidate(candidate ICECandidateMsg) error
	OnICEServers(servers []webrtc.ICEServer) error
}

// SignalingServer bridges a browser JS hook with a Go Pion PeerConnection
// via WebSocket on a configurable port.
type SignalingServer struct {
	handler SignalingHandler
	logFn   LogFunc
	mu      sync.Mutex
	ws      *websocket.Conn
}

// NewSignalingServer creates a signaling server with the given handler.
func NewSignalingServer(handler SignalingHandler, logFn LogFunc) *SignalingServer {
	if logFn == nil {
		logFn = func(msg string) { log.Print(msg) }
	}
	return &SignalingServer{
		handler: handler,
		logFn:   logFn,
	}
}

// Start begins listening for signaling connections on the given port. Blocks.
func (s *SignalingServer) Start(port int) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/signaling", s.handleWS)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	s.logFn(fmt.Sprintf("[signaling] listening on %s", addr))

	return http.ListenAndServe(addr, mux)
}

// SendToHook sends a signaling message to the connected browser hook.
func (s *SignalingServer) SendToHook(msg SignalingMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws == nil {
		return fmt.Errorf("no hook connected")
	}
	return s.ws.WriteJSON(msg)
}

func (s *SignalingServer) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := sigUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logFn(fmt.Sprintf("[signaling] upgrade error: %v", err))
		return
	}
	s.mu.Lock()
	s.ws = ws
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.ws = nil
		s.mu.Unlock()
		ws.Close()
	}()

	s.logFn("[signaling] hook connected")

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			s.logFn(fmt.Sprintf("[signaling] read error: %v", err))
			return
		}

		var msg SignalingMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.logFn(fmt.Sprintf("[signaling] parse error: %v", err))
			continue
		}

		s.processMessage(msg)
	}
}

func (s *SignalingServer) processMessage(msg SignalingMessage) {
	switch msg.Type {
	case "offer":
		answer, err := s.handler.OnOffer(msg.SDP)
		if err != nil {
			s.logFn(fmt.Sprintf("[signaling] offer error: %v", err))
			return
		}
		if answer != nil {
			s.SendToHook(SignalingMessage{
				Type:    "answer",
				SDP:     answer.SDP,
				SDPType: "answer",
			})
		}

	case "answer":
		if err := s.handler.OnAnswer(msg.SDP); err != nil {
			s.logFn(fmt.Sprintf("[signaling] answer error: %v", err))
		}

	case "candidate":
		if msg.Candidate != nil {
			if err := s.handler.OnCandidate(*msg.Candidate); err != nil {
				s.logFn(fmt.Sprintf("[signaling] candidate error: %v", err))
			}
		}

	case "config":
		servers := ParseICEServers(msg.Servers)
		if err := s.handler.OnICEServers(servers); err != nil {
			s.logFn(fmt.Sprintf("[signaling] config error: %v", err))
		}

	default:
		s.logFn(fmt.Sprintf("[signaling] unknown message type: %s", msg.Type))
	}
}
