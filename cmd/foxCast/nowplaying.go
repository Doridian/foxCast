package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

// cmdNowPlaying shows what a receiver is playing, read only.
func cmdNowPlaying(ctx context.Context, args []string) error {
	var opts connectOptions
	flags := flag.NewFlagSet("nowplaying", flag.ContinueOnError)
	opts.register(flags)
	wait := flags.Duration("wait", 5*time.Second, "how long to listen for state updates")
	raw := flags.Bool("raw", false, "dump every message the receiver sends as decoded protobuf")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := opts.finish(1); err != nil {
		return err
	}
	conn, err := connect(ctx, &opts, false)
	if err != nil {
		return err
	}
	defer conn.client.Close()

	rc, err := conn.client.OpenRemoteControl(ctx)
	if err != nil {
		return fmt.Errorf("remote control: %w", err)
	}
	defer rc.Close()

	timeout := time.After(*wait)
	var latest sender.NowPlaying
	var have bool
	for {
		select {
		case message, ok := <-rc.Messages():
			if !ok {
				return fmt.Errorf("connection closed: %w", rc.Err())
			}
			if *raw {
				fmt.Printf("---\n%s", sender.DumpProtobuf(message))
			}
			if np, ok := sender.ParseNowPlaying(message); ok {
				latest, have = np, true
			}
		case <-timeout:
			if !have {
				return errors.New("the receiver reported no playback state")
			}
			fmt.Printf("%+v\n", latest)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
