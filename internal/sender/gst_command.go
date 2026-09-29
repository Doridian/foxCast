package sender

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"git.foxden.network/FoxDen/foxCast/internal/gst"
)

// gstPollInterval bounds how long a stopped pipeline's watcher takes to notice.
const gstPollInterval = 100 * time.Millisecond

var errGstStopped = errors.New("gst: stopped")

// gstCommand runs a gst-launch-1.0 style pipeline inside this process. It
// mirrors the parts of exec.Cmd the capture code used when GStreamer ran as
// a child: arguments are gst-launch arguments, "fd=1" is the stdout pipe
// returned by StdoutPipe, and "fd=3", "fd=4", ... are ExtraFiles.
//
// Shutting a pipeline down can block: pipewiresrc that has failed takes
// about 30 s to reach NULL. So the pipeline counts as ended (Wait returns)
// as soon as it fails, stops or finishes, and it is torn down in the
// background. Its descriptors stay open until that completes, so GStreamer
// never writes to a closed pipe, which would raise SIGPIPE on its thread.
type gstCommand struct {
	ctx        context.Context
	Args       []string
	ExtraFiles []*os.File

	stdoutR, stdoutW *os.File
	extra            []*os.File // our duplicates of ExtraFiles

	stateMu sync.Mutex
	started bool
	stopped bool

	// pipeMu guards pipeline, which the watcher clears when the pipeline
	// ends; users hold the read lock while they touch it.
	pipeMu   sync.RWMutex
	pipeline *gst.Pipeline

	done   chan struct{} // the pipeline has ended; err is set
	err    error
	closed chan struct{} // the pipeline is torn down and its descriptors closed
}

func newGstCommand(ctx context.Context, args ...string) *gstCommand {
	return &gstCommand{ctx: ctx, Args: args, done: make(chan struct{}), closed: make(chan struct{})}
}

// StdoutPipe returns the reading end of the pipeline's "fd=1".
func (c *gstCommand) StdoutPipe() (io.ReadCloser, error) {
	if c.stdoutR != nil {
		return nil, errors.New("gst: stdout pipe already requested")
	}
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	// fdsink expects a blocking descriptor; the reading end stays
	// non-blocking so Go's poller can interrupt a read.
	if err := unix.SetNonblock(fds[1], false); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	c.stdoutR = os.NewFile(uintptr(fds[0]), "gst-stdout")
	c.stdoutW = os.NewFile(uintptr(fds[1]), "gst-stdout-writer")
	return &gstStdout{File: c.stdoutR, cmd: c}, nil
}

// gstStdout is the reading end of a pipeline's stdout. Close stops the
// pipeline and interrupts reads at once, but closes the descriptor only after
// the pipeline has let go of the writing end.
type gstStdout struct {
	*os.File
	cmd  *gstCommand
	once sync.Once
}

func (s *gstStdout) Close() error {
	s.once.Do(func() {
		if !s.cmd.running() {
			_ = s.File.Close()
			return
		}
		s.cmd.Kill()
		_ = s.SetReadDeadline(time.Now())
		go func() {
			<-s.cmd.closed
			_ = s.File.Close()
		}()
	})
	return nil
}

// launchArgs maps gst-launch options and file descriptors to this process.
func (c *gstCommand) launchArgs() ([]string, error) {
	fds := map[string]string{}
	if c.stdoutW != nil {
		fds["fd=1"] = "fd=" + strconv.Itoa(int(c.stdoutW.Fd()))
	}
	for i, f := range c.ExtraFiles {
		// Duplicate, so the caller may close its file right after Start.
		dup, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("gst: duplicate fd: %w", err)
		}
		c.extra = append(c.extra, os.NewFile(uintptr(dup), f.Name()))
		fds["fd="+strconv.Itoa(3+i)] = "fd=" + strconv.Itoa(dup)
	}
	args := make([]string, 0, len(c.Args))
	for _, arg := range c.Args {
		switch arg {
		case "--quiet", "-q", "-e", "--eos-on-shutdown":
			continue
		}
		if mapped, ok := fds[arg]; ok {
			arg = mapped
		}
		args = append(args, arg)
	}
	return args, nil
}

// Start builds and plays the pipeline.
func (c *gstCommand) Start() error {
	args, err := c.launchArgs()
	var pipeline *gst.Pipeline
	if err == nil {
		dbg("[GST] pipeline: %s", strings.Join(args, " "))
		pipeline, err = gst.ParseLaunch(args)
	}
	if err == nil {
		if err = pipeline.Play(); err != nil {
			pipeline.Close()
		}
	}
	if err != nil {
		c.closeFiles()
		close(c.closed)
		return err
	}
	c.pipeline = pipeline
	c.stateMu.Lock()
	c.started = true
	c.stateMu.Unlock()
	go c.watch(pipeline)
	return nil
}

func (c *gstCommand) running() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.started
}

func (c *gstCommand) watch(pipeline *gst.Pipeline) {
	var err error
	for err == nil {
		select {
		case <-c.ctx.Done():
			err = c.ctx.Err()
		default:
			err = pipeline.Poll(gstPollInterval)
		}
		c.stateMu.Lock()
		if c.stopped {
			err = errGstStopped
		}
		c.stateMu.Unlock()
	}
	if errors.Is(err, gst.ErrEOS) || errors.Is(err, errGstStopped) || errors.Is(err, context.Canceled) {
		err = nil
	}
	c.err = err
	close(c.done)

	c.pipeMu.Lock()
	c.pipeline = nil
	c.pipeMu.Unlock()
	pipeline.Close()
	c.closeFiles()
	close(c.closed)
}

// closeFiles closes the write end of stdout, so readers see EOF, and the
// duplicated descriptors.
func (c *gstCommand) closeFiles() {
	if c.stdoutW != nil {
		_ = c.stdoutW.Close()
	}
	closeFiles(c.extra)
}

// Wait blocks until the pipeline ends, returning its error (nil for EOS or
// Kill). Teardown may still be in progress.
func (c *gstCommand) Wait() error {
	<-c.done
	return c.err
}

// Kill stops the pipeline.
func (c *gstCommand) Kill() {
	c.stateMu.Lock()
	c.stopped = true
	c.stateMu.Unlock()
}

// Element returns a named element of the running pipeline, for properties
// that change while it plays. Callers release it.
func (c *gstCommand) Element(name string) *gst.Element {
	var e *gst.Element
	_ = c.withPipeline(func(p *gst.Pipeline) error {
		e = p.Element(name)
		return nil
	})
	return e
}

// withPipeline runs f on the running pipeline, which stays alive until f
// returns. It returns errGstStopped once the pipeline has ended.
func (c *gstCommand) withPipeline(f func(*gst.Pipeline) error) error {
	c.pipeMu.RLock()
	defer c.pipeMu.RUnlock()
	if c.pipeline == nil {
		return errGstStopped
	}
	return f(c.pipeline)
}

// startGStreamerCommand starts cmd and returns a channel that receives its
// result when it ends.
func startGStreamerCommand(cmd *gstCommand) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- cmd.Wait()
		close(waitResult)
	}()
	return waitResult, nil
}
