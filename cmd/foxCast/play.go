package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/fileserver"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// playStopTimeout bounds the /stop + TEARDOWN exchange after Ctrl+C.
const playStopTimeout = 5 * time.Second

func cmdPlay(ctx context.Context, args []string) error {
	var opts connectOptions
	flags := flag.NewFlagSet("play", flag.ContinueOnError)
	opts.register(flags)
	start := flags.Float64("start", 0, "start position in seconds")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: foxCast play [flags] <url|file>\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("expected exactly one URL or file")
	}
	if err := opts.finish(1); err != nil {
		return err
	}
	media := flags.Arg(0)

	conn, err := connect(ctx, &opts, true)
	if err != nil {
		return err
	}
	defer conn.Close()

	url := media
	if _, statErr := os.Stat(media); statErr == nil {
		fs, err := fileserver.Start(ctx, media)
		if err != nil {
			return fmt.Errorf("file server: %w", err)
		}
		defer fs.Shutdown()
		url, err = fs.URL(conn.addr)
		if err != nil {
			return fmt.Errorf("file URL: %w", err)
		}
		log.Printf("serving %s at %s", media, url)
	}

	log.Printf("playing %s", url)
	session, err := conn.client.PlayURL(ctx, url, sender.PlaybackConfig{
		StartSeconds: *start,
		PortMin:      opts.portMin,
		PortMax:      opts.portMax,
	})
	if err != nil {
		return err
	}
	fmt.Println("Playback started. Press Ctrl+C to stop.")

	err = session.Wait(ctx)
	if errors.Is(err, context.Canceled) {
		fmt.Println("\nStopping...")
		err = nil
	}
	closed := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(playStopTimeout):
		log.Println("warning: receiver did not acknowledge stop")
	}
	return err
}
