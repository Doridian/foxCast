package sender

import (
	"io"
	"os"
	"testing"
	"time"
)

// readFadeAlphas reads overlay frames until the fader closes its pipe or n
// frames arrive, returning each frame's alpha.
func readFadeAlphas(t *testing.T, r *os.File, n int) []byte {
	t.Helper()
	var alphas []byte
	frame := make([]byte, fadeOverlayPipe)
	for len(alphas) < n {
		if _, err := io.ReadFull(r, frame); err != nil {
			break
		}
		for i := 3; i < len(frame); i += 4 {
			if frame[i] != frame[3] {
				t.Fatalf("frame %d has mixed alpha %d and %d", len(alphas), frame[3], frame[i])
			}
		}
		alphas = append(alphas, frame[3])
	}
	return alphas
}

func TestVideoFaderHoldsThenFadesInAndCloses(t *testing.T) {
	f, r, err := newVideoFader(30, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	alphas := readFadeAlphas(t, r, 1000)
	if len(alphas) < 3+15 {
		t.Fatalf("got %d frames, want the hold and a 15-frame fade: %v", len(alphas), alphas)
	}
	for i := range 3 {
		if alphas[i] != 255 {
			t.Fatalf("hold frame %d alpha = %d, want 255", i, alphas[i])
		}
	}
	for i := 4; i < len(alphas); i++ {
		if alphas[i] > alphas[i-1] {
			t.Fatalf("alpha rose during fade-in: %v", alphas)
		}
	}
	if last := alphas[len(alphas)-1]; last != 0 {
		t.Fatalf("last alpha = %d, want 0", last)
	}
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("fader did not exit after fading in")
	}
	select {
	case <-f.fadeOut():
	default:
		t.Fatal("fadeOut on a finished overlay did not report at once")
	}
}

func TestVideoFaderFadeOutReportsAfterSettling(t *testing.T) {
	f, r, err := newVideoFader(30, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer f.Close()
	if alphas := readFadeAlphas(t, r, 20); alphas[len(alphas)-1] != 0 {
		t.Fatalf("placeholder did not fade in: %v", alphas)
	}
	done := f.fadeOut()
	frames := 0
	frame := make([]byte, fadeOverlayPipe)
	for {
		select {
		case <-done:
			// 15 fade frames plus 10 settle frames, give or take the frames
			// already queued in the pipe.
			if frames < 20 || frames > 30 {
				t.Fatalf("fadeOut reported after %d frames", frames)
			}
			if frame[3] != 255 {
				t.Fatalf("fadeOut reported at alpha %d", frame[3])
			}
			return
		default:
		}
		if _, err := io.ReadFull(r, frame); err != nil {
			t.Fatalf("read after %d frames: %v", frames, err)
		}
		frames++
		if frames > 100 {
			t.Fatal("fadeOut never reported")
		}
	}
}

func TestWithFadeOverlayNamesCompositor(t *testing.T) {
	stages := withFadeOverlay([]gstStage{
		{"videoscale"},
		{"compositor", "force-live=true"},
	}, 1920, 1080)
	want := gstStage{"compositor", "force-live=true", "name=fade", "sink_1::zorder=1", "sink_1::width=1920", "sink_1::height=1080"}
	if got := stages[1]; len(got) != len(want) {
		t.Fatalf("compositor stage = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("compositor stage = %v, want %v", got, want)
			}
		}
	}
	if len(stages[0]) != 1 {
		t.Fatalf("non-compositor stage changed: %v", stages[0])
	}
}
