package gst

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func waitEnd(t *testing.T, p *Pipeline) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.Poll(100 * time.Millisecond); err != nil {
			return err
		}
	}
	t.Fatal("pipeline did not end")
	return nil
}

func TestPipelineRunsToEOS(t *testing.T) {
	p, err := ParseLaunch([]string{"videotestsrc", "num-buffers=5", "!", "fakesink"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Play(); err != nil {
		t.Fatal(err)
	}
	if err := waitEnd(t, p); !errors.Is(err, ErrEOS) {
		t.Fatalf("end = %v, want EOS", err)
	}
}

func TestParseLaunchRejectsUnknownElement(t *testing.T) {
	if _, err := ParseLaunch([]string{"foxcast-no-such-element"}); err == nil {
		t.Fatal("unknown element parsed")
	}
	// gst_parse_launchv returns a pipeline alongside this error.
	if _, err := ParseLaunch([]string{"videotestsrc", "!", "foxcast-no-such-element", "!", "fakesink"}); err == nil {
		t.Fatal("pipeline with an unknown element parsed")
	}
	if HasElement("foxcast-no-such-element") || !HasElement("videotestsrc") {
		t.Fatal("HasElement is wrong")
	}
}

func TestPipelineReportsErrors(t *testing.T) {
	p, err := ParseLaunch([]string{"videotestsrc", "!", "video/x-raw,format=NV12", "!", "audioconvert", "!", "fakesink"})
	if err != nil {
		t.Skipf("pipeline rejected at parse time: %v", err)
	}
	defer p.Close()
	_ = p.Play()
	if err := waitEnd(t, p); err == nil || errors.Is(err, ErrEOS) {
		t.Fatalf("end = %v, want an error", err)
	}
}

func TestAddBinToPlayingCompositor(t *testing.T) {
	p, err := ParseLaunch([]string{
		"compositor", "name=mix", "force-live=true", "background=black",
		"!", "video/x-raw,width=64,height=36,framerate=30/1", "!", "fakesink", "sync=false",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Play(); err != nil {
		t.Fatal(err)
	}
	mix := p.Element("mix")
	if mix == nil {
		t.Fatal("mixer not found")
	}
	defer mix.Release()

	bin, err := p.AddBin("videotestsrc is-live=true ! capsfilter caps=video/x-raw,width=64,height=36")
	if err != nil {
		t.Fatal(err)
	}
	src, err := bin.StaticPad("src")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Release()
	sink, err := mix.RequestPad("sink_%u")
	if err != nil {
		t.Fatal(err)
	}
	sink.Set("alpha", "0.5")
	sink.CountBuffers()
	if err := src.Link(sink); err != nil {
		t.Fatal(err)
	}
	if err := bin.SyncState(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.Buffers() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("no buffers reached the mixer")
		}
		if err := p.Poll(10 * time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
	p.RemoveBin(bin)
	sink.Release()
	if err := p.Poll(200 * time.Millisecond); err != nil && !strings.Contains(err.Error(), "not-linked") {
		t.Fatalf("pipeline failed after removing the bin: %v", err)
	}
}
