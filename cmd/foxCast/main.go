package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/airplay"
	"git.foxden.network/FoxDen/foxCast/internal/fileserver"
	"git.foxden.network/FoxDen/foxCast/internal/mdns"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "discover":
		err = cmdDiscover()
	case "pair":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: foxCast pair <host:port>\n")
			os.Exit(1)
		}
		err = cmdPair(os.Args[2])
	case "play":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "usage: foxCast play <host:port> <url>\n")
			os.Exit(1)
		}
		err = cmdPlay(os.Args[2], os.Args[3])
	case "cast":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: foxCast cast <file>\n")
			os.Exit(1)
		}
		err = cmdCast(os.Args[2])
	default:
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `foxCast — AirPlay video URL sender

Usage:
  foxCast discover               Scan for AirPlay receivers on the local network
  foxCast pair   <host:port>     Pair with a receiver (saves credentials)
  foxCast play   <host:port> <url>  Play a URL on a paired receiver
  foxCast cast   <file>          Discover, serve, and cast a local file

`)
}

// cmdDiscover scans for AirPlay receivers on the local network for 5 seconds
// and prints what it finds.
func cmdDiscover() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	devices, err := mdns.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}

	found := 0
	for dev := range devices {
		found++
		fmt.Printf("%-30s  %s  model=%-14s  deviceid=%s\n",
			dev.Name, dev.Addr(), dev.Model, dev.DeviceID)
	}
	if found == 0 {
		fmt.Println("No AirPlay receivers found.")
	}
	return nil
}

// cmdPair connects to addr, runs pair-setup (prompting for PIN), and saves credentials.
func cmdPair(addr string) error {
	// Minimal fake Device for connecting to a known address.
	dev := &mdns.Device{
		Host:     hostFromAddr(addr),
		Port:     portFromAddr(addr),
		DeviceID: addr, // use addr as key until we get the real device ID from /info
	}

	fmt.Printf("Connecting to %s ...\n", addr)
	fmt.Print("Enter PIN shown on receiver: ")
	pin := readLine()
	if pin == "" {
		return fmt.Errorf("no PIN entered")
	}

	client, err := airplay.Connect(dev, pin)
	if err != nil {
		return err
	}
	defer client.Close()

	info, err := client.GetInfo()
	if err != nil {
		fmt.Printf("Warning: could not fetch /info: %v\n", err)
	} else {
		fmt.Printf("Paired with %s (%s)\n", info.Name, info.Model)
	}
	return nil
}

// cmdPlay connects to addr and plays url.
func cmdPlay(addr, url string) error {
	dev := &mdns.Device{
		Host:     hostFromAddr(addr),
		Port:     portFromAddr(addr),
		DeviceID: addr,
	}

	fmt.Printf("Connecting to %s ...\n", addr)
	client, err := airplay.Connect(dev, "")
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Printf("Playing: %s\n", url)
	if err := client.Play(url, 0.0); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	fmt.Println("Playback started. Press Ctrl+C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nStopping...")
	_ = client.Stop()
	return nil
}

// cmdCast discovers a receiver via mDNS, starts a local file server, and plays the file.
func cmdCast(file string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	// Discover receivers.
	fmt.Println("Scanning for AirPlay receivers...")
	discCtx, discCancel := context.WithTimeout(ctx, 5*time.Second)
	defer discCancel()

	devices, err := mdns.Discover(discCtx)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}

	var dev *mdns.Device
	for d := range devices {
		fmt.Printf("Found: %s at %s\n", d.Name, d.Addr())
		dev = d
		break // use the first one found
	}
	if dev == nil {
		return fmt.Errorf("no AirPlay receivers found")
	}

	// Start local file server.
	fmt.Printf("Serving %s ...\n", file)
	fs, err := fileserver.Start(ctx, file)
	if err != nil {
		return fmt.Errorf("file server: %w", err)
	}
	defer fs.Shutdown()

	fileURL, err := fs.URL(dev.Addr())
	if err != nil {
		return fmt.Errorf("file URL: %w", err)
	}

	// Connect and play.
	fmt.Printf("Connecting to %s ...\n", dev.Addr())
	client, err := airplay.Connect(dev, "")
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Close()

	fmt.Printf("Playing: %s\n", fileURL)
	if err := client.Play(fileURL, 0.0); err != nil {
		return fmt.Errorf("play: %w", err)
	}
	fmt.Println("Playback started. Press Ctrl+C to stop.")

	<-ctx.Done()
	fmt.Println("\nStopping...")
	_ = client.Stop()
	return nil
}

// hostFromAddr extracts the host from "host:port" (falls back to the full string).
func hostFromAddr(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

// portFromAddr extracts the port number from "host:port" (returns 7000 if missing).
func portFromAddr(addr string) int {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			var p int
			fmt.Sscan(addr[i+1:], &p)
			if p > 0 {
				return p
			}
		}
	}
	return 7000
}

// readLine reads a single line from stdin.
func readLine() string {
	var line string
	fmt.Fscanln(os.Stdin, &line)
	return strings.TrimSpace(line)
}
