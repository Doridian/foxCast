package main

// Continuing what an Apple TV is playing on this computer: the inverse of
// opening a link in an Apple TV app. See docs/12-nowplaying.md.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/sender"
	"git.foxden.network/FoxDen/foxCast/internal/ytsearch"
)

const (
	// handoffStateTimeout bounds waiting for the receiver to report playback
	// after connecting.
	handoffStateTimeout = 6 * time.Second
	// handoffPauseTimeout bounds waiting for the pause to be reflected.
	handoffPauseTimeout = 3 * time.Second

	youTubeBundleID = "com.google.ios.youtube"
)

// handoffResult describes a completed handoff.
type handoffResult struct {
	Title    string
	URL      string
	Position float64
	Paused   bool
}

// handoff reads what the receiver is playing, finds the video on YouTube,
// pauses the receiver (when pause is set) and returns the link to open at the
// paused position. Only YouTube is supported.
func handoff(ctx context.Context, opts *connectOptions, pause bool) (*handoffResult, error) {
	conn, err := connect(ctx, opts, false)
	if err != nil {
		return nil, err
	}
	defer conn.client.Close()

	rc, err := conn.client.OpenRemoteControl(ctx)
	if err != nil {
		return nil, fmt.Errorf("remote control: %w", err)
	}
	defer rc.Close()

	waitCtx, cancel := context.WithTimeout(ctx, handoffStateTimeout)
	np, err := rc.WaitState(waitCtx, func(n sender.NowPlaying) bool { return n.State == "playing" || n.State == "paused" })
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s is not playing anything", conn.info.Name)
		}
		return nil, err
	}
	if np.BundleID != youTubeBundleID {
		return nil, fmt.Errorf("%s is playing %q in %s; only YouTube can be continued here", conn.info.Name, np.Title, appName(np))
	}

	// Look the video up before pausing, so a failed lookup leaves the
	// receiver playing.
	video, err := ytsearch.Find(ctx, http.DefaultClient, ytsearch.Query{Title: np.Title, Channel: np.Artist, Duration: np.Duration})
	if err != nil {
		return nil, fmt.Errorf("%q: %w", np.Title, err)
	}

	res := &handoffResult{Title: video.Title}
	if pause && np.State == "playing" {
		if err := rc.Command(ctx, sender.MRPCommandPause); err != nil {
			return nil, fmt.Errorf("pause: %w", err)
		}
		res.Paused = true
		waitCtx, cancel := context.WithTimeout(ctx, handoffPauseTimeout)
		updated, err := rc.WaitState(waitCtx, func(n sender.NowPlaying) bool { return n.State == "paused" })
		cancel()
		if err == nil {
			np = updated
		} else {
			// Not reflected in time: the position is extrapolated to now.
			np.State = "paused"
		}
	}
	res.Position = np.Position(time.Now())
	res.URL = ytsearch.WatchURL(video.ID, res.Position)
	return res, nil
}

func appName(np sender.NowPlaying) string {
	if np.App != "" {
		return np.App
	}
	return np.BundleID
}

// openBrowser opens url in the default browser.
func openBrowser(url string) error {
	if err := exec.Command("xdg-open", url).Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}

// cmdHandoff continues the receiver's current YouTube video in a local
// browser at the same position, pausing the receiver.
func cmdHandoff(ctx context.Context, args []string) error {
	var opts connectOptions
	flags := flag.NewFlagSet("handoff", flag.ContinueOnError)
	opts.register(flags)
	noPause := flags.Bool("no-pause", false, "leave the receiver playing")
	printOnly := flags.Bool("print", false, "print the link instead of opening it in the browser")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: foxCast handoff [flags]\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := opts.finish(1); err != nil {
		return err
	}
	res, err := handoff(ctx, &opts, !*noPause)
	if err != nil {
		return err
	}
	fmt.Printf("%s at %d:%02d\n", res.Title, int(res.Position)/60, int(res.Position)%60)
	if *printOnly {
		fmt.Println(res.URL)
		return nil
	}
	return openBrowser(res.URL)
}
