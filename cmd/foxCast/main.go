package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "discover":
		err = cmdDiscover(ctx)
	case "pair":
		err = cmdPair(ctx, args)
	case "play":
		err = cmdPlay(ctx, args)
	case "mirror":
		err = cmdMirror(ctx, args)
	case "group":
		err = cmdGroup(ctx, args)
	case "probe":
		err = cmdProbe(ctx, args)
	case "serve":
		err = cmdServe(ctx, args)
	case "gui":
		err = cmdGUI(ctx, args)
	case "-h", "-help", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `foxCast — AirPlay sender

Usage:
  foxCast discover                     Scan for AirPlay receivers
  foxCast pair   [flags]               Pair with a receiver (saves credentials)
  foxCast play   [flags] <url|file>    Play a URL or local file on a receiver
  foxCast mirror [flags]               Mirror the screen to a receiver, or audio to a speaker
  foxCast group  [flags] <receiver>... Play audio on several receivers in step (stereo pair, surround)
  foxCast probe  [flags] <url|file>    Show how a file would be played (tracks, transmuxing)
  foxCast serve  [flags] <url|file>    Serve a file (as HLS if it is Matroska) without a receiver
  foxCast gui    [flags]               System tray app

Run "foxCast <command> -h" for command flags.
`)
}

func cmdDiscover(ctx context.Context) error {
	devices, err := discover(ctx)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	if len(devices) == 0 {
		fmt.Println("No AirPlay receivers found.")
		return nil
	}
	for _, d := range devices {
		fmt.Printf("%-30s  %s:%d  model=%-18s  deviceid=%s\n", d.Name, d.IP, d.Port, d.Model, d.DeviceID)
	}
	return nil
}

func cmdPair(ctx context.Context, args []string) error {
	var opts connectOptions
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	opts.register(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := opts.finish(0); err != nil {
		return err
	}
	opts.forcePair = true
	conn, err := connect(ctx, &opts, false)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Printf("Paired with %s (%s)\n", conn.info.Name, conn.info.Model)
	return nil
}
