package main

// RTMP ingest: the receiver shows a placeholder at once, and whatever a
// local client (such as OBS) publishes to the RTMP listener plays on it.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/rtmp"
	"git.foxden.network/FoxDen/foxCast/internal/sender"
)

const (
	defaultRTMPListen = "127.0.0.1:1935"
	// rtmpTimeout drops a client that sends nothing for this long, so the
	// placeholder returns when a publisher hangs rather than disconnects.
	rtmpTimeout = 10 * time.Second
)

func cmdRTMP(ctx context.Context, args []string) error {
	var opts connectOptions
	var mo mirrorOptions
	flags := flag.NewFlagSet("rtmp", flag.ContinueOnError)
	opts.register(flags)
	mo.registerStream(flags)
	listen := flags.String("listen", defaultRTMPListen, "RTMP listen address; loopback only")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := mo.finish(); err != nil {
		return err
	}
	if err := opts.finish(mirrorUDPPorts); err != nil {
		return err
	}
	// Bind before connecting, so a taken port fails at once.
	ln, err := listenRTMP(*listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	mo.ingest = ln
	// Stream audio plays into this receiver's own output device, which is
	// left alone as the desktop's default.
	mo.audioSource, mo.keepDefaultSink = audioSourceSink, true
	log.Printf("publish to %s (any stream key) once the receiver shows the placeholder", "rtmp://"+ln.Addr().String()+"/live")
	defer opts.openPTP()()
	return runMirror(ctx, &opts, &mo, mirrorHooks{})
}

// listenRTMP listens on addr, which must be a loopback address: RTMP has no
// authentication, so anyone who can reach the port could put a picture on
// the receiver.
func listenRTMP(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid -listen: %w", err)
	}
	if host != "localhost" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("invalid -listen %q: RTMP ingest only listens on loopback addresses", addr)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("RTMP listen: %w", err)
	}
	return ln, nil
}

// serveIngest plays each publishing client on display until ctx ends. A new
// publisher replaces the current one, so a client that reconnects before
// its old connection has timed out takes over at once.
func serveIngest(ctx context.Context, ln net.Listener, display *sender.IngestDisplay) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				log.Printf("RTMP accept: %v", err)
			}
			return
		}
		go func() {
			remote := conn.RemoteAddr()
			publisher, err := rtmp.Accept(conn, rtmpTimeout)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("RTMP client %s: %v", remote, err)
				}
				return
			}
			log.Printf("RTMP client %s publishing %q to %q", remote, publisher.Key, publisher.App)
			if err := display.Play(ctx, publisher); err != nil {
				log.Printf("RTMP stream from %s: %v", remote, err)
			}
			log.Printf("RTMP client %s done", remote)
		}()
	}
}
