package sender

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// virtualSinkPrefix namespaces the sinks foxCast creates so a sink left
	// behind by a killed process can be recognized and replaced.
	virtualSinkPrefix = "foxcast_"
	virtualSinkModule = "module-null-sink"
)

// pactlRunner runs pactl with the given arguments and returns its stdout.
type pactlRunner func(args ...string) ([]byte, error)

func runPactl(args ...string) ([]byte, error) {
	out, err := exec.Command("pactl", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return out, fmt.Errorf("pactl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return out, fmt.Errorf("pactl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// VirtualSink is an audio output device that exists for the duration of a
// mirror session. Applications play into it like any speaker; its monitor
// source feeds the AirPlay audio stream. It is a PulseAudio null sink, which
// pipewire-pulse serves the same way, so one path covers both sound servers.
type VirtualSink struct {
	name            string
	moduleID        string
	previousDefault string
	pactl           pactlRunner
}

// VirtualSinkName returns the sink name used for a receiver device ID.
func VirtualSinkName(deviceID string) string {
	var b strings.Builder
	b.WriteString(virtualSinkPrefix)
	for _, r := range strings.ToLower(deviceID) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CreateVirtualSink creates a stereo 44.1 kHz sink named name, shown to the
// user as description. With makeDefault it also becomes the default output,
// which moves streams that follow the default onto it, until Close.
func CreateVirtualSink(name, description string, makeDefault bool) (*VirtualSink, error) {
	return createVirtualSink(runPactl, name, description, makeDefault)
}

func createVirtualSink(pactl pactlRunner, name, description string, makeDefault bool) (*VirtualSink, error) {
	if err := unloadStaleVirtualSinks(pactl, name); err != nil {
		return nil, err
	}
	out, err := pactl(virtualSinkLoadArgs(name, description)...)
	if err != nil {
		return nil, fmt.Errorf("create virtual sink: %w", err)
	}
	sink := &VirtualSink{
		name:     name,
		moduleID: strings.TrimSpace(string(out)),
		pactl:    pactl,
	}
	dbg("[AUDIO] created virtual sink %s (module %s)", name, sink.moduleID)
	if !makeDefault {
		return sink, nil
	}
	previous, err := pactl("get-default-sink")
	if err != nil {
		sink.Close()
		return nil, fmt.Errorf("read default sink: %w", err)
	}
	sink.previousDefault = strings.TrimSpace(string(previous))
	if _, err := pactl("set-default-sink", name); err != nil {
		sink.previousDefault = ""
		sink.Close()
		return nil, fmt.Errorf("make virtual sink the default: %w", err)
	}
	dbg("[AUDIO] default sink %s -> %s", sink.previousDefault, name)
	return sink, nil
}

func virtualSinkLoadArgs(name, description string) []string {
	// Module arguments are parsed by the sound server. PulseAudio only keeps
	// spaces in a value quoted as a whole, so the property list is wrapped in
	// single quotes around the double-quoted description; pipewire-pulse
	// accepts the same form. Quotes and backslashes in a receiver name would
	// end either quoted value early.
	description = strings.NewReplacer(`"`, "", `\`, "", `'`, "").Replace(description)
	return []string{
		"load-module", virtualSinkModule,
		"sink_name=" + name,
		fmt.Sprintf("rate=%d", audioSampleRate),
		fmt.Sprintf("channels=%d", audioChannels),
		"channel_map=front-left,front-right",
		fmt.Sprintf(`sink_properties='device.description="%s"'`, description),
	}
}

// unloadStaleVirtualSinks removes null-sink modules with this sink name. They
// are left behind when a previous session was killed before Close ran.
func unloadStaleVirtualSinks(pactl pactlRunner, name string) error {
	out, err := pactl("list", "short", "modules")
	if err != nil {
		return fmt.Errorf("list sound server modules: %w", err)
	}
	for _, id := range staleVirtualSinkModules(string(out), name) {
		dbg("[AUDIO] unloading stale virtual sink %s (module %s)", name, id)
		if _, err := pactl("unload-module", id); err != nil {
			return fmt.Errorf("unload stale virtual sink: %w", err)
		}
	}
	return nil
}

func staleVirtualSinkModules(list, name string) []string {
	var ids []string
	for _, line := range strings.Split(list, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 || fields[1] != virtualSinkModule {
			continue
		}
		for _, arg := range strings.Fields(fields[2]) {
			if arg == "sink_name="+name {
				ids = append(ids, fields[0])
				break
			}
		}
	}
	return ids
}

// Name returns the sink name.
func (s *VirtualSink) Name() string { return s.name }

// MonitorSource returns the source that records what is played into the sink.
func (s *VirtualSink) MonitorSource() string { return s.name + ".monitor" }

// Close restores the previous default output and removes the sink. The
// default is restored first so streams move back to it rather than to
// whichever sink the sound server would pick as a fallback.
func (s *VirtualSink) Close() error {
	var errs []error
	if s.previousDefault != "" && s.previousDefault != s.name {
		if _, err := s.pactl("set-default-sink", s.previousDefault); err != nil {
			errs = append(errs, fmt.Errorf("restore default sink: %w", err))
		}
		s.previousDefault = ""
	}
	if s.moduleID != "" {
		if _, err := s.pactl("unload-module", s.moduleID); err != nil {
			errs = append(errs, fmt.Errorf("remove virtual sink: %w", err))
		}
		s.moduleID = ""
	}
	return errors.Join(errs...)
}
