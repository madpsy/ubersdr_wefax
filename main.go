// main.go — ubersdr_wefax: multi-channel HF weather-fax decoder
//
// Usage:
//
//	ubersdr_wefax -url ws://sdr.example.com/ws \
//	              -channel 7880000:usb \
//	              -channel 13882500:usb \
//	              -listen :8080 \
//	              -output ./images
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// channelFlag is a repeatable -channel flag value.
type channelFlag []string

func (c *channelFlag) String() string { return strings.Join(*c, ", ") }
func (c *channelFlag) Set(v string) error {
	*c = append(*c, v)
	return nil
}

func main() {
	var (
		ubersdrURL  = flag.String("url", "", "UberSDR WebSocket URL (e.g. ws://host/ws)")
		password    = flag.String("password", "", "UberSDR password (optional)")
		listenAddr  = flag.String("listen", ":8080", "HTTP listen address")
		outputDir   = flag.String("output", "./images", "Directory to save decoded images")
		lpm         = flag.Int("lpm", 120, "Lines per minute (120 or 60)")
		imageWidth  = flag.Int("width", 1809, "Image width in pixels (IOC-576=1809, IOC-288=904)")
		noPhasing   = flag.Bool("no-phasing", false, "Disable phasing (horizontal sync)")
		noAutoStop  = flag.Bool("no-autostop", false, "Disable auto-stop on STOP tone")
		noAutoStart = flag.Bool("no-autostart", false, "Disable auto-start on START tone")
		uiPassword  = flag.String("ui-password", envOr("UI_PASSWORD", ""),
			"Password required for write actions in the web UI (env: UI_PASSWORD; empty = write actions disabled)")

		cleanupPartialDays = flag.Int("cleanup-partial-days", envIntOr("CLEANUP_PARTIAL_DAYS", 7),
			"Delete partial images (< 95% decoded) older than N days; 0 = disabled (env: CLEANUP_PARTIAL_DAYS)")
		cleanupSNRDays = flag.Int("cleanup-snr-days", envIntOr("CLEANUP_SNR_DAYS", 7),
			"Delete low-SNR images (below the passband-SNR threshold, ~5.3 dB avg) older than N days; 0 = disabled (env: CLEANUP_SNR_DAYS)")
		cleanupAllDays = flag.Int("cleanup-all-days", envIntOr("CLEANUP_ALL_DAYS", 30),
			"Delete ALL images older than N days regardless of quality; 0 = disabled (env: CLEANUP_ALL_DAYS)")
	)

	var channels channelFlag
	flag.Var(&channels, "channel", "Frequency:mode to decode, e.g. 7880000:usb (repeatable)")

	flag.Parse()

	if *ubersdrURL == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(1)
	}
	if len(channels) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one -channel is required")
		flag.Usage()
		os.Exit(1)
	}

	cfg := DefaultWEFAXConfig()
	cfg.LPM = *lpm
	cfg.ImageWidth = *imageWidth
	cfg.UsePhasing = !*noPhasing
	cfg.AutoStop = !*noAutoStop
	cfg.AutoStart = !*noAutoStart

	log.Printf("[main] ubersdr_wefax starting")
	log.Printf("[main] UberSDR URL: %s", *ubersdrURL)
	log.Printf("[main] Output dir:  %s", *outputDir)
	log.Printf("[main] Listen addr: %s", *listenAddr)
	log.Printf("[main] WEFAX config: LPM=%d Width=%d Phasing=%v AutoStop=%v AutoStart=%v",
		cfg.LPM, cfg.ImageWidth, cfg.UsePhasing, cfg.AutoStop, cfg.AutoStart)

	hub := newSSEHub()
	store := newImageStore(*outputDir, hub)

	// Start background cleanup workers (no-ops when their day threshold is 0).
	startPartialCleanup(store, *outputDir, *cleanupPartialDays)
	startSNRCleanup(store, *outputDir, *cleanupSNRDays)
	startAgeCleanup(store, *outputDir, *cleanupAllDays)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Parse and start each channel.
	var wefaxChannels []*wefaxChannel
	for _, spec := range channels {
		freqHz, mode, err := parseChannelSpec(spec)
		if err != nil {
			log.Fatalf("[main] invalid -channel %q: %v", spec, err)
		}
		inst := newInstance(freqHz, int(cfg.Carrier), mode, *ubersdrURL, *password)
		ch := newWefaxChannel(inst, cfg, store, hub)
		wefaxChannels = append(wefaxChannels, ch)
		log.Printf("[main] starting channel %s (%d Hz, %s)", inst.label, freqHz, mode)
		go ch.run(ctx, cfg)
	}

	// Start HTTP server in background.
	go func() {
		if err := startHTTPServer(*listenAddr, store, hub, wefaxChannels, *uiPassword); err != nil {
			log.Fatalf("[main] HTTP server: %v", err)
		}
	}()

	// Wait for SIGINT / SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("[main] shutting down…")
	cancel()
	log.Printf("[main] done")
}

// parseChannelSpec parses "7880000:usb" → (7880000, "usb", nil).
func parseChannelSpec(spec string) (int, string, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("expected freq:mode, got %q", spec)
	}
	freq, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", fmt.Errorf("invalid frequency %q: %w", parts[0], err)
	}
	mode := strings.TrimSpace(parts[1])
	if mode == "" {
		return 0, "", fmt.Errorf("empty mode in %q", spec)
	}
	return freq, mode, nil
}
