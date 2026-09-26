package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/fileserver"
	"git.foxden.network/FoxDen/foxCast/internal/mediasource"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
	"git.foxden.network/FoxDen/foxCast/internal/transmux"
)

// playStopTimeout bounds the /stop + TEARDOWN exchange after Ctrl+C.
const playStopTimeout = 5 * time.Second

// transmuxFlags are the flags shared by play and probe.
type transmuxFlags struct {
	enabled     bool
	audio       string
	dolbyVision bool
}

func (t *transmuxFlags) register(flags *flag.FlagSet) {
	flags.BoolVar(&t.enabled, "transmux", true, "remux Matroska (MKV/WebM) files to HLS so the receiver can play them")
	flags.StringVar(&t.audio, "audio", "", "comma-separated Matroska audio track numbers to offer (first is the default); see \"foxCast probe\"")
	flags.BoolVar(&t.dolbyVision, "dolby-vision", true, "keep Dolby Vision profile 5/8 metadata (otherwise play the HDR10/SDR base layer)")
}

func (t *transmuxFlags) options() (transmux.Options, error) {
	opts := transmux.DefaultOptions()
	opts.DolbyVision = t.dolbyVision
	opts.Logf = func(format string, args ...any) {
		if sender.DebugMode() {
			log.Printf(format, args...)
		}
	}
	if t.audio != "" {
		for _, f := range strings.Split(t.audio, ",") {
			n, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
			if err != nil {
				return opts, fmt.Errorf("-audio: invalid track number %q", f)
			}
			opts.AudioTracks = append(opts.AudioTracks, n)
		}
	}
	return opts, nil
}

// media is what the receiver will be told to play: either a URL it fetches
// itself, or a handler foxCast serves.
type media struct {
	url     string
	handler http.Handler
	urlPath string
	session *transmux.Session
	close   func()
}

// prepareMedia decides how to present location to the receiver.
func prepareMedia(ctx context.Context, location string, tf *transmuxFlags) (*media, error) {
	remote := mediasource.IsRemote(location)
	local := false
	if !remote {
		if _, err := os.Stat(location); err == nil {
			local = true
		}
	}
	if !remote && !local {
		// Other URL schemes go to the receiver as-is; anything else is a
		// local path that does not exist.
		if strings.Contains(location, "://") {
			return &media{url: location, close: func() {}}, nil
		}
		return nil, fmt.Errorf("%s: no such file", location)
	}

	if tf.enabled {
		src, err := mediasource.Open(ctx, location)
		switch {
		case err != nil && remote:
			// Not range-capable (or unreachable from here): let the receiver try.
			log.Printf("not probing %s: %v", location, err)
		case err != nil:
			return nil, err
		case transmux.IsMatroska(src):
			opts, err := tf.options()
			if err != nil {
				src.Close()
				return nil, err
			}
			session, err := transmux.NewSession(ctx, src, opts)
			if err != nil {
				src.Close()
				return nil, fmt.Errorf("transmux %s: %w", src.Name(), err)
			}
			return &media{handler: session, urlPath: session.MasterPath(), session: session, close: func() { src.Close() }}, nil
		default:
			src.Close()
		}
	}

	if remote {
		return &media{url: location, close: func() {}}, nil
	}
	handler, urlPath, err := fileserver.FileHandler(location)
	if err != nil {
		return nil, err
	}
	return &media{handler: handler, urlPath: urlPath, close: func() {}}, nil
}

func printSession(s *transmux.Session) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TRACK\tTYPE\tCODEC\tLANG\tNAME\tUSE")
	for _, t := range s.Tracks() {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.Number, t.Type, t.Codec, t.Language, t.Name, t.Status)
	}
	tw.Flush()
	for _, n := range s.Notes() {
		fmt.Printf("note: %s\n", n)
	}
}

func cmdPlay(ctx context.Context, args []string) error {
	var opts connectOptions
	var tf transmuxFlags
	flags := flag.NewFlagSet("play", flag.ContinueOnError)
	opts.register(flags)
	tf.register(flags)
	start := flags.Float64("start", 0, "start position in seconds")
	httpPort := flags.Int("http-port", 0, "local TCP port the receiver fetches local/transmuxed media from (0 = random; fix it to open it in a firewall)")
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

	// Probe the media before connecting so unsupported files fail fast.
	m, err := prepareMedia(ctx, flags.Arg(0), &tf)
	if err != nil {
		return err
	}
	defer m.close()
	if m.session != nil {
		printSession(m.session)
	}

	// URL playback needs no FairPlay setup (pyatv does none).
	conn, err := connect(ctx, &opts, false)
	if err != nil {
		return err
	}
	defer conn.Close()

	url := m.url
	if m.handler != nil {
		srv, err := fileserver.Start(ctx, m.handler, *httpPort)
		if err != nil {
			return fmt.Errorf("file server: %w", err)
		}
		defer srv.Shutdown()
		url, err = srv.URL(conn.addr, m.urlPath)
		if err != nil {
			return fmt.Errorf("file URL: %w", err)
		}
		log.Printf("serving %s at %s", flags.Arg(0), url)
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

// cmdProbe shows how a file would be played without connecting.
func cmdProbe(ctx context.Context, args []string) error {
	var tf transmuxFlags
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	tf.register(flags)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: foxCast probe [flags] <url|file>\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("expected exactly one URL or file")
	}
	tf.enabled = true
	m, err := prepareMedia(ctx, flags.Arg(0), &tf)
	if err != nil {
		return err
	}
	defer m.close()
	if m.session == nil {
		fmt.Println("Not a Matroska file; it will be passed to the receiver as-is.")
		return nil
	}
	printSession(m.session)
	fmt.Printf("Duration %v; served as HLS (fMP4) without re-encoding.\n", m.session.Duration().Round(time.Second))
	return nil
}

// cmdServe serves a file (transmuxed when needed) over HTTP without a
// receiver, for testing with other players (Safari, VLC, ffplay).
func cmdServe(ctx context.Context, args []string) error {
	var tf transmuxFlags
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	tf.register(flags)
	listen := flags.String("listen", "127.0.0.1:8080", "address to listen on")
	debug := flags.Bool("debug", false, "log requests and segment reads")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: foxCast serve [flags] <url|file>\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("expected exactly one URL or file")
	}
	sender.SetDebugMode(*debug)
	m, err := prepareMedia(ctx, flags.Arg(0), &tf)
	if err != nil {
		return err
	}
	defer m.close()
	if m.handler == nil {
		return errors.New("nothing to serve: remote non-Matroska URLs are played directly")
	}
	if m.session != nil {
		printSession(m.session)
	}
	srv := &http.Server{Addr: *listen, Handler: m.handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	fmt.Printf("Serving at http://%s%s\n", *listen, m.urlPath)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
