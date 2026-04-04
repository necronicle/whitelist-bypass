package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"whitelist-bypass/relay/mobile"
	"whitelist-bypass/relay/pion"
)

type stdLogger struct{}

func (s stdLogger) OnLog(msg string) {
	log.Print(msg)
}

func main() {
	mode := flag.String("mode", "", "joiner or creator")
	wsPort := flag.Int("ws-port", 9000, "WebSocket port for browser connection")
	socksPort := flag.Int("socks-port", 1080, "SOCKS5 proxy port (joiner mode only)")
	transport := flag.String("transport", "dc", "transport mode: dc or vp8")
	sigPort := flag.Int("sig-port", 9001, "signaling WebSocket port (vp8 mode only)")
	platform := flag.String("platform", "vk", "platform: vk or telemost (vp8 mode only)")
	flag.Parse()

	if *mode == "" {
		fmt.Fprintf(os.Stderr, "Usage: relay --mode joiner|creator [--transport dc|vp8] [--platform vk|telemost]\n")
		os.Exit(1)
	}

	cb := stdLogger{}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	switch *transport {
	case "dc":
		runDC(*mode, *wsPort, *socksPort, cb, sig)
	case "vp8":
		runVP8(*mode, *wsPort, *socksPort, *sigPort, *platform, sig)
	default:
		fmt.Fprintf(os.Stderr, "Unknown transport: %s (use dc or vp8)\n", *transport)
		os.Exit(1)
	}
}

func runDC(mode string, wsPort, socksPort int, cb stdLogger, sig chan os.Signal) {
	var relay *mobile.Relay
	var err error

	switch mode {
	case "joiner":
		relay, err = mobile.StartJoiner(wsPort, socksPort, cb)
	case "creator":
		relay, err = mobile.StartCreator(wsPort, cb)
	default:
		fmt.Fprintf(os.Stderr, "Unknown mode: %s (use joiner or creator)\n", mode)
		os.Exit(1)
	}

	if err != nil {
		log.Fatal(err)
	}

	<-sig
	log.Print("Shutting down...")
	relay.Stop()
}

func runVP8(mode string, wsPort, socksPort, sigPort int, platform string, sig chan os.Signal) {
	logFn := pion.LogFunc(func(msg string) { log.Print(msg) })

	// Create relay bridge with nil tunnel — it will be set when the
	// platform client establishes the WebRTC connection.
	bridge := pion.NewRelayBridge(nil, mode, logFn)

	// Create platform-specific client.
	var sigServer *pion.SignalingServer
	switch platform {
	case "vk":
		client := pion.NewVKClient(bridge, logFn)
		sigServer = client.SignalingServer()
	case "telemost":
		client := pion.NewTelemostClient(bridge, logFn)
		sigServer = client.SignalingServer()
	default:
		fmt.Fprintf(os.Stderr, "Unknown platform: %s (use vk or telemost)\n", platform)
		os.Exit(1)
	}

	// Start signaling server.
	go func() {
		if err := sigServer.Start(sigPort); err != nil {
			log.Fatalf("signaling server: %v", err)
		}
	}()

	// For joiner mode, start SOCKS5 proxy.
	if mode == "joiner" {
		go func() {
			if err := bridge.StartSOCKS5(socksPort); err != nil {
				log.Fatalf("SOCKS5: %v", err)
			}
		}()
	}

	log.Printf("VP8 mode: %s/%s, signaling on :%d", mode, platform, sigPort)

	<-sig
	log.Print("Shutting down...")
	bridge.Stop()
}
