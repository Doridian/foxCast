// Package gst is a thin CGo binding to the parts of GStreamer foxCast uses:
// building pipelines from gst-launch style descriptions, running them,
// setting element and pad properties, and adding and removing source bins
// while a pipeline plays.
package gst

/*
#cgo pkg-config: gstreamer-1.0
#include <stdlib.h>
#include <gst/gst.h>

// fc_parse_launchv treats every parse error as fatal, like gst-launch-1.0:
// a missing element still yields a pipeline, just one that cannot flow.
static GstElement *fc_parse_launchv(char **argv, char **err) {
	GError *gerr = NULL;
	GstElement *e = gst_parse_launchv((const gchar **)argv, &gerr);
	if (gerr != NULL) {
		*err = g_strdup(gerr->message);
		g_error_free(gerr);
		if (e != NULL) {
			gst_object_unref(e);
		}
		return NULL;
	}
	return e;
}

// fc_parse_bin parses a bin and ghosts only its unlinked source pad as
// "src". Ghosting every unlinked pad would also expose optional inputs such
// as textoverlay's text_sink, which then waits for a text stream forever.
static GstElement *fc_parse_bin(const char *desc, char **err) {
	GError *gerr = NULL;
	GstElement *e = gst_parse_bin_from_description(desc, FALSE, &gerr);
	if (gerr != NULL) {
		*err = g_strdup(gerr->message);
		g_error_free(gerr);
		if (e != NULL) {
			gst_object_unref(e);
		}
		return NULL;
	}
	GstPad *pad = gst_bin_find_unlinked_pad(GST_BIN(e), GST_PAD_SRC);
	if (pad == NULL) {
		*err = g_strdup("bin has no unlinked source pad");
		gst_object_unref(e);
		return NULL;
	}
	gst_element_add_pad(e, gst_ghost_pad_new("src", pad));
	gst_object_unref(pad);
	return e;
}

static int fc_wait_playing(GstElement *e, int timeout_ms) {
	GstState state = GST_STATE_NULL;
	gst_element_get_state(e, &state, NULL, (GstClockTime)timeout_ms * GST_MSECOND);
	return state == GST_STATE_PLAYING;
}

static int fc_set_state(GstElement *e, GstState state) {
	return gst_element_set_state(e, state) != GST_STATE_CHANGE_FAILURE;
}

// fc_pop waits up to timeout_ms for an error or end-of-stream message and
// returns 0 (none), 1 (EOS) or 2 (error, with *text set).
static int fc_pop(GstElement *pipeline, int timeout_ms, char **text) {
	GstBus *bus = gst_element_get_bus(pipeline);
	GstMessage *msg = gst_bus_timed_pop_filtered(bus, (GstClockTime)timeout_ms * GST_MSECOND,
		GST_MESSAGE_ERROR | GST_MESSAGE_EOS);
	gst_object_unref(bus);
	if (msg == NULL) {
		return 0;
	}
	int kind = 1;
	if (GST_MESSAGE_TYPE(msg) == GST_MESSAGE_ERROR) {
		GError *gerr = NULL;
		gchar *debug = NULL;
		gst_message_parse_error(msg, &gerr, &debug);
		*text = g_strdup_printf("%s: %s%s%s", GST_OBJECT_NAME(msg->src), gerr->message,
			debug ? " (" : "", debug ? debug : "");
		if (debug) {
			gchar *closed = g_strconcat(*text, ")", NULL);
			g_free(*text);
			*text = closed;
		}
		g_error_free(gerr);
		g_free(debug);
		kind = 2;
	}
	gst_message_unref(msg);
	return kind;
}

static GstElement *fc_by_name(GstElement *bin, const char *name) {
	return gst_bin_get_by_name(GST_BIN(bin), name);
}

static void fc_set(gpointer obj, const char *name, const char *value) {
	gst_util_set_object_arg(G_OBJECT(obj), name, value);
}

static int fc_has_property(gpointer obj, const char *name) {
	return g_object_class_find_property(G_OBJECT_GET_CLASS(obj), name) != NULL;
}

static int fc_add(GstElement *bin, GstElement *e) {
	return gst_bin_add(GST_BIN(bin), e);
}

static int fc_remove(GstElement *bin, GstElement *e) {
	return gst_bin_remove(GST_BIN(bin), e);
}

static GstPad *fc_request_pad(GstElement *e, const char *templ) {
	return gst_element_request_pad_simple(e, templ);
}

static GstPad *fc_static_pad(GstElement *e, const char *name) {
	return gst_element_get_static_pad(e, name);
}

static int fc_link(GstPad *src, GstPad *sink) {
	return gst_pad_link(src, sink) == GST_PAD_LINK_OK;
}

static GstPadProbeReturn fc_count_buffer(GstPad *pad, GstPadProbeInfo *info, gpointer data) {
	g_atomic_int_inc((gint *)data);
	return GST_PAD_PROBE_OK;
}

static gint *fc_count_buffers(GstPad *pad) {
	gint *count = g_new0(gint, 1);
	// The probe owns count and frees it when the pad goes away.
	gst_pad_add_probe(pad, GST_PAD_PROBE_TYPE_BUFFER, fc_count_buffer, count, g_free);
	return count;
}

static int fc_buffers(gint *count) {
	return g_atomic_int_get(count);
}

static int fc_has_factory(const char *name) {
	GstElementFactory *f = gst_element_factory_find(name);
	if (f == NULL) {
		return 0;
	}
	gst_object_unref(f);
	return 1;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// initOnce guards gst_init, which is process-wide state by nature.
var initOnce sync.Once

// Init initializes GStreamer. It is safe to call more than once.
func Init() {
	initOnce.Do(func() { C.gst_init(nil, nil) })
}

// ErrEOS is returned by Pipeline.Poll when the pipeline ends normally.
var ErrEOS = errors.New("end of stream")

// HasElement reports whether an element factory is installed.
func HasElement(name string) bool {
	Init()
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return C.fc_has_factory(cname) != 0
}

func takeError(cerr *C.char, what string) error {
	if cerr == nil {
		return fmt.Errorf("%s failed", what)
	}
	defer C.g_free(C.gpointer(cerr))
	return fmt.Errorf("%s: %s", what, C.GoString(cerr))
}

// Pipeline is a playing or paused GStreamer pipeline.
type Pipeline struct {
	e *C.GstElement
}

// ParseLaunch builds a pipeline from gst-launch-1.0 style arguments.
func ParseLaunch(args []string) (*Pipeline, error) {
	Init()
	argv := make([]*C.char, len(args)+1)
	for i, arg := range args {
		argv[i] = C.CString(arg)
	}
	defer func() {
		for _, arg := range argv[:len(args)] {
			C.free(unsafe.Pointer(arg))
		}
	}()
	// The argv array must live in C memory: it holds C pointers.
	carray := (**C.char)(C.malloc(C.size_t(len(argv)) * C.size_t(unsafe.Sizeof(argv[0]))))
	defer C.free(unsafe.Pointer(carray))
	copy(unsafe.Slice(carray, len(argv)), argv)
	var cerr *C.char
	e := C.fc_parse_launchv(carray, &cerr)
	if e == nil {
		return nil, takeError(cerr, "parse pipeline")
	}
	return &Pipeline{e: e}, nil
}

// Play starts the pipeline.
func (p *Pipeline) Play() error {
	if C.fc_set_state(p.e, C.GST_STATE_PLAYING) == 0 {
		return errors.New("pipeline refused to play")
	}
	return nil
}

// WaitPlaying waits up to timeout for the pipeline to reach PLAYING. Bins
// added while the state change is still in progress may not follow it.
func (p *Pipeline) WaitPlaying(timeout time.Duration) error {
	if C.fc_wait_playing(p.e, C.int(timeout/time.Millisecond)) == 0 {
		return errors.New("pipeline did not start playing")
	}
	return nil
}

// Close stops the pipeline and releases it. It blocks until its streaming
// threads have stopped.
func (p *Pipeline) Close() {
	C.fc_set_state(p.e, C.GST_STATE_NULL)
	C.gst_object_unref(C.gpointer(p.e))
}

// Poll waits up to timeout for the pipeline to fail or end, returning nil if
// it is still running, ErrEOS, or its error.
func (p *Pipeline) Poll(timeout time.Duration) error {
	var text *C.char
	switch C.fc_pop(p.e, C.int(timeout/time.Millisecond), &text) {
	case 1:
		return ErrEOS
	case 2:
		defer C.g_free(C.gpointer(text))
		return errors.New(C.GoString(text))
	}
	return nil
}

// Element returns the named element of the pipeline, or nil.
func (p *Pipeline) Element(name string) *Element {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	e := C.fc_by_name(p.e, cname)
	if e == nil {
		return nil
	}
	return &Element{e: e}
}

// AddBin parses a bin description (with its unlinked source pad ghosted as
// "src"), adds it to the pipeline, and returns it. Link it, then call
// SyncState.
func (p *Pipeline) AddBin(desc string) (*Element, error) {
	cdesc := C.CString(desc)
	defer C.free(unsafe.Pointer(cdesc))
	var cerr *C.char
	e := C.fc_parse_bin(cdesc, &cerr)
	if e == nil {
		return nil, takeError(cerr, "parse bin")
	}
	C.gst_object_ref(C.gpointer(e)) // keep a reference past gst_bin_add's sink
	if C.fc_add(p.e, e) == 0 {
		C.gst_object_unref(C.gpointer(e))
		C.gst_object_unref(C.gpointer(e))
		return nil, errors.New("add bin to pipeline")
	}
	return &Element{e: e}, nil
}

// RemoveBin stops a bin added with AddBin and removes it from the pipeline.
func (p *Pipeline) RemoveBin(bin *Element) {
	C.fc_set_state(bin.e, C.GST_STATE_NULL)
	C.fc_remove(p.e, bin.e)
	bin.Release()
}

// Element is a reference to a GStreamer element.
type Element struct {
	e *C.GstElement
}

// Release drops the reference.
func (e *Element) Release() {
	if e != nil && e.e != nil {
		C.gst_object_unref(C.gpointer(e.e))
		e.e = nil
	}
}

// Set sets a property from its string form, as gst-launch would.
func (e *Element) Set(name, value string) {
	setProperty(C.gpointer(e.e), name, value)
}

// HasProperty reports whether the element has the property.
func (e *Element) HasProperty(name string) bool {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return C.fc_has_property(C.gpointer(e.e), cname) != 0
}

// SyncState brings an element added to a playing pipeline to its state.
func (e *Element) SyncState() error {
	if C.gst_element_sync_state_with_parent(e.e) == 0 {
		return errors.New("element refused to follow the pipeline state")
	}
	return nil
}

// RequestPad requests a pad from a template such as "sink_%u".
func (e *Element) RequestPad(template string) (*Pad, error) {
	ctempl := C.CString(template)
	defer C.free(unsafe.Pointer(ctempl))
	pad := C.fc_request_pad(e.e, ctempl)
	if pad == nil {
		return nil, fmt.Errorf("request pad %s", template)
	}
	return &Pad{p: pad, owner: e}, nil
}

// StaticPad returns an always pad, such as a bin's ghosted "src".
func (e *Element) StaticPad(name string) (*Pad, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	pad := C.fc_static_pad(e.e, cname)
	if pad == nil {
		return nil, fmt.Errorf("no pad %s", name)
	}
	return &Pad{p: pad}, nil
}

// Pad is a reference to a pad. A requested pad remembers its element so it
// can be released.
type Pad struct {
	p     *C.GstPad
	owner *Element
	count *C.gint
}

// Link links a source pad to a sink pad.
func (p *Pad) Link(sink *Pad) error {
	if C.fc_link(p.p, sink.p) == 0 {
		return errors.New("link pads")
	}
	return nil
}

// Set sets a pad property from its string form.
func (p *Pad) Set(name, value string) {
	setProperty(C.gpointer(p.p), name, value)
}

// CountBuffers starts counting the buffers that pass the pad; Buffers
// returns the count.
func (p *Pad) CountBuffers() {
	if p.count == nil {
		p.count = C.fc_count_buffers(p.p)
	}
}

// Buffers returns how many buffers have passed since CountBuffers.
func (p *Pad) Buffers() int {
	if p.count == nil {
		return 0
	}
	return int(C.fc_buffers(p.count))
}

// Unref drops the reference without releasing a requested pad, for a pad
// whose pipeline is shutting down.
func (p *Pad) Unref() {
	if p == nil || p.p == nil {
		return
	}
	C.gst_object_unref(C.gpointer(p.p))
	p.p = nil
	p.count = nil
}

// Release releases a requested pad back to its element, or drops the
// reference to any other pad.
func (p *Pad) Release() {
	if p == nil || p.p == nil {
		return
	}
	if p.owner != nil && p.owner.e != nil {
		C.gst_element_release_request_pad(p.owner.e, p.p)
	}
	C.gst_object_unref(C.gpointer(p.p))
	p.p = nil
	p.count = nil
}

func setProperty(obj C.gpointer, name, value string) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	cvalue := C.CString(value)
	defer C.free(unsafe.Pointer(cvalue))
	C.fc_set(obj, cname, cvalue)
}
