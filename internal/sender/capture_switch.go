package sender

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
)

// CaptureSwitcher changes what a Wayland mirror session shares: it starts on
// the placeholder, and each Switch shows the portal picker and moves the
// session's compositor to the chosen screen or window, fading the old one out
// and the new one in (see capture_mixer.go).
type CaptureSwitcher struct {
	base  *CapturePreparation
	mixer *videoMixer
	ctx   context.Context // session lifetime

	// pick and sourceBin are the portal and PipeWire hooks; tests replace them.
	pick      func(ctx context.Context, restoreToken string) (*portalSource, error)
	sourceBin func(src *portalSource, width, height, fps int) (string, []*os.File, error)

	mu      sync.Mutex // serializes Switch and Close
	current *mixerSource
	portal  *portalSource // the session behind current; nil for the placeholder
	closed  bool
}

// NewCaptureSwitcher takes over capture, the placeholder session started from
// base with DeferSource.
func NewCaptureSwitcher(ctx context.Context, base *CapturePreparation, capture *ScreenCapture) (*CaptureSwitcher, error) {
	if !base.CanSwitchSource() || capture.mixer == nil {
		return nil, errors.New("capture source can only be switched in a Wayland session capture")
	}
	m := capture.mixer
	m.mu.Lock()
	var current *mixerSource
	if len(m.sources) > 0 {
		current = m.sources[0]
	}
	m.mu.Unlock()
	return &CaptureSwitcher{
		base:      base,
		mixer:     m,
		ctx:       ctx,
		pick:      base.pickPortalSource,
		sourceBin: portalSourceBin,
		current:   current,
	}, nil
}

// portalSourceBin reads the portal session's node through its own PipeWire
// connection.
func portalSourceBin(src *portalSource, width, height, fps int) (string, []*os.File, error) {
	remote, err := src.openRemote()
	if err != nil {
		return "", nil, err
	}
	return pipeWireBin(int(remote.Fd()), src.nodeID, width, height, fps), []*os.File{remote}, nil
}

// Switch shows the portal picker (cancellable with pickCtx; restore lets
// -remember-source skip it) and moves the session to the chosen source.
// Dismissing the picker returns ErrPortalCancelled and changes nothing.
func (s *CaptureSwitcher) Switch(pickCtx context.Context, restore bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errGstStopped
	}
	restoreToken := ""
	if restore {
		s.base.mu.Lock()
		restoreToken = s.base.cfg.RestoreToken
		s.base.mu.Unlock()
	}
	next, err := s.pick(pickCtx, restoreToken)
	if err != nil {
		return err
	}
	desc, files, err := s.sourceBin(next, s.mixer.width, s.mixer.height, s.mixer.fps)
	if err != nil {
		next.Close()
		return fmt.Errorf("open screen capture: %w", err)
	}
	dbg("[CAPTURE] switch: source picked (node %d); fading out", next.nodeID)
	s.mixer.stopIntro()

	// Add the new source, invisible, before removing the old one: a
	// compositor left without pads restarts its output timeline. It also
	// starts up while the old one fades out.
	source, err := s.mixer.add(desc, files...)
	if err != nil {
		next.Close()
		return fmt.Errorf("start screen capture: %w", err)
	}
	if s.current != nil {
		if err := s.mixer.fade(s.ctx, s.current, 1, 0); err != nil {
			s.mixer.remove(source, next.Close)
			return err
		}
		s.mixer.remove(s.current, s.portal.Close)
	}
	s.current, s.portal = source, next
	if err := s.mixer.fadeIn(s.ctx, source); err != nil {
		return fmt.Errorf("start screen capture: %w", err)
	}
	log.Printf("[CAPTURE] now sharing PipeWire node %d", next.nodeID)
	return nil
}

// Close ends the portal session. The capture itself stops with its broadcast.
func (s *CaptureSwitcher) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.portal.Close()
	s.portal = nil
}
