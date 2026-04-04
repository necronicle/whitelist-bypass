// Headless creator — runs a VK Call creator without a browser.
// Authenticates via VK cookies, creates a call, and tunnels traffic
// through VP8 video frames using Pion WebRTC.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"whitelist-bypass/relay/pion"

	"github.com/pion/webrtc/v4"
)

func main() {
	cookiesFile := flag.String("cookies", "", "path to cookies file (Netscape format)")
	platform := flag.String("platform", "vk", "platform: vk (only vk supported for headless)")
	socksPort := flag.Int("socks-port", 1080, "SOCKS5 proxy port")
	resource := flag.String("resource", "default", "resource mode: moderate, default, unlimited")
	flag.Parse()

	if *cookiesFile == "" {
		fmt.Fprintf(os.Stderr, "Usage: headless --cookies FILE [--platform vk] [--resource moderate|default|unlimited]\n")
		os.Exit(1)
	}
	if *platform != "vk" {
		fmt.Fprintf(os.Stderr, "Only vk platform is supported for headless mode\n")
		os.Exit(1)
	}

	// Set resource limits.
	switch *resource {
	case "moderate":
		debug.SetMemoryLimit(64 << 20) // 64 MB
		log.Print("Resource mode: moderate (64 MB)")
	case "unlimited":
		log.Print("Resource mode: unlimited")
	default:
		debug.SetMemoryLimit(128 << 20) // 128 MB
		log.Print("Resource mode: default (128 MB)")
	}

	// Read cookies.
	cookiesRaw, err := os.ReadFile(*cookiesFile)
	if err != nil {
		log.Fatalf("Read cookies: %v", err)
	}

	// Authenticate with VK.
	log.Print("Authenticating with VK...")
	api, err := NewVKAPIClient(string(cookiesRaw))
	if err != nil {
		log.Fatalf("Create API client: %v", err)
	}
	if err := api.Authenticate(); err != nil {
		log.Fatalf("Authenticate: %v", err)
	}
	log.Print("Authenticated successfully")

	// Create call.
	log.Print("Creating call...")
	callInfo, err := api.StartCall()
	if err != nil {
		log.Fatalf("Start call: %v", err)
	}
	fmt.Printf("\n=== JOIN LINK ===\n%s\n=================\n\n", callInfo.JoinLink)
	log.Printf("Call ID: %s", callInfo.CallID)

	// Set up VP8 tunnel.
	logFn := pion.LogFunc(func(msg string) { log.Print(msg) })
	bridge := pion.NewRelayBridge(nil, "creator", logFn)

	// Convert ICE servers to Pion format.
	var iceServers []webrtc.ICEServer
	for _, s := range callInfo.ICEServers {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}

	// Create VK client with Pion.
	client := pion.NewVKClient(bridge, logFn)
	if err := client.OnICEServers(iceServers); err != nil {
		log.Fatalf("Configure ICE: %v", err)
	}

	// Start signaling server for the joiner to connect.
	go func() {
		if err := client.SignalingServer().Start(9001); err != nil {
			log.Fatalf("Signaling server: %v", err)
		}
	}()

	// Start SOCKS5 proxy (not typical for headless creator, but useful for testing).
	_ = socksPort

	log.Print("Headless creator running. Waiting for joiner...")
	log.Print("Signaling server on :9001")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	log.Print("Shutting down...")
	client.Close()
	bridge.Stop()
}
