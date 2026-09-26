package sender

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakePactl struct {
	calls   [][]string
	outputs map[string]string
	fail    map[string]bool
}

func (f *fakePactl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	for prefix := range f.fail {
		if strings.HasPrefix(key, prefix) {
			return nil, errors.New("pactl failed")
		}
	}
	for prefix, out := range f.outputs {
		if strings.HasPrefix(key, prefix) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (f *fakePactl) commands() []string {
	var cmds []string
	for _, call := range f.calls {
		if call[0] == "load-module" {
			cmds = append(cmds, "load-module")
			continue
		}
		cmds = append(cmds, strings.Join(call, " "))
	}
	return cmds
}

func TestVirtualSinkName(t *testing.T) {
	if got := VirtualSinkName("8A:32:E1:28:77:96"); got != "foxcast_8a32e1287796" {
		t.Fatalf("VirtualSinkName = %q", got)
	}
}

func TestVirtualSinkLoadArgs(t *testing.T) {
	got := virtualSinkLoadArgs("foxcast_x", `Living "room" TV\ (foxCast)`)
	want := []string{
		"load-module", "module-null-sink",
		"sink_name=foxcast_x",
		"rate=44100",
		"channels=2",
		"channel_map=front-left,front-right",
		`sink_properties='device.description="Living room TV (foxCast)"'`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("load args = %q, want %q", got, want)
	}
}

func TestStaleVirtualSinkModules(t *testing.T) {
	list := "536870912\tmodule-always-sink\t\n" +
		"536870916\tmodule-null-sink\tsink_name=foxcast_x rate=44100 sink_properties=device.description=\"TV\"\n" +
		"536870917\tmodule-null-sink\tsink_name=foxcast_xy rate=44100\n" +
		"536870918\tmodule-loopback\tsink_name=foxcast_x\n"
	got := staleVirtualSinkModules(list, "foxcast_x")
	if !reflect.DeepEqual(got, []string{"536870916"}) {
		t.Fatalf("stale modules = %q", got)
	}
}

func TestVirtualSinkMakesDefaultAndRestoresItBeforeUnload(t *testing.T) {
	pactl := &fakePactl{outputs: map[string]string{
		"list short modules": "7\tmodule-null-sink\tsink_name=foxcast_x\n",
		"load-module":        "42\n",
		"get-default-sink":   "speakers\n",
	}}
	sink, err := createVirtualSink(pactl.run, "foxcast_x", "TV", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sink.MonitorSource() != "foxcast_x.monitor" {
		t.Fatalf("monitor = %q", sink.MonitorSource())
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	want := []string{
		"list short modules",
		"unload-module 7",
		"load-module",
		"get-default-sink",
		"set-default-sink foxcast_x",
		"set-default-sink speakers",
		"unload-module 42",
	}
	if got := pactl.commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pactl calls = %q, want %q", got, want)
	}
}

func TestVirtualSinkWithoutDefaultLeavesDefaultAlone(t *testing.T) {
	pactl := &fakePactl{outputs: map[string]string{"load-module": "42\n"}}
	sink, err := createVirtualSink(pactl.run, "foxcast_x", "TV", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := []string{"list short modules", "load-module", "unload-module 42"}
	if got := pactl.commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pactl calls = %q, want %q", got, want)
	}
}

func TestVirtualSinkRemovedWhenDefaultSwitchFails(t *testing.T) {
	pactl := &fakePactl{
		outputs: map[string]string{"load-module": "42\n", "get-default-sink": "speakers\n"},
		fail:    map[string]bool{"set-default-sink": true},
	}
	if _, err := createVirtualSink(pactl.run, "foxcast_x", "TV", true); err == nil {
		t.Fatal("create succeeded, want error")
	}
	calls := pactl.commands()
	if last := calls[len(calls)-1]; last != "unload-module 42" {
		t.Fatalf("last pactl call = %q, want the sink removed; calls %q", last, calls)
	}
}
