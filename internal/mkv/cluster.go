package mkv

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Lacing modes from the block flags.
const (
	lacingNone  = 0
	lacingXiph  = 1
	lacingFixed = 2
	lacingEBML  = 3

	simpleBlockKeyframe = 0x80

	clusterReaderBuffer = 1 << 20
)

// Frame is one decoded (unlaced, content-decoded) frame.
type Frame struct {
	Track    uint64
	PTS      time.Duration
	Duration time.Duration // BlockDuration if present, else 0
	Keyframe bool
	Data     []byte
	Pos      Position
}

// ClusterReader streams frames from a byte stream that begins at a Cluster
// (or any top-level element) inside the Segment.
type ClusterReader struct {
	f   *File
	br  *bufio.Reader
	pos int64 // absolute offset of the next unread byte

	// Tracks limits which tracks' frame data is read; nil reads all.
	Tracks map[uint64]bool
	// StopAt, when set, ends the stream at the first block at or after it.
	StopAt *Position

	inCluster   bool
	clusterPos  int64
	clusterData int64
	clusterEnd  int64 // unknownSize when not known
	clusterTS   int64

	pending []*Frame
}

// NewClusterReader reads frames from r, whose first byte is at absolute
// offset start.
func (f *File) NewClusterReader(r io.Reader, start int64) *ClusterReader {
	return &ClusterReader{f: f, br: bufio.NewReaderSize(r, clusterReaderBuffer), pos: start}
}

func (c *ClusterReader) readHeader() (id uint32, size int64, err error) {
	b, err := c.br.Peek(maxHeaderLen)
	if len(b) == 0 {
		if err == nil {
			err = io.EOF
		}
		return 0, 0, err
	}
	id, size, n, err := parseHeader(b)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, 0, io.EOF
		}
		return 0, 0, err
	}
	if _, err := c.br.Discard(n); err != nil {
		return 0, 0, err
	}
	c.pos += int64(n)
	return id, size, nil
}

func (c *ClusterReader) skip(n int64) error {
	for n > 0 {
		step := n
		if step > 1<<30 {
			step = 1 << 30
		}
		d, err := c.br.Discard(int(step))
		c.pos += int64(d)
		n -= int64(d)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *ClusterReader) readBody(size int64) ([]byte, error) {
	buf := make([]byte, size)
	n, err := io.ReadFull(c.br, buf)
	c.pos += int64(n)
	return buf, err
}

func isTopLevel(id uint32) bool {
	switch id {
	case idCluster, idCues, idTags, idChapters, idAttachments, idSeekHead, idInfo, idTracks:
		return true
	}
	return false
}

// Next returns the next frame, or io.EOF.
func (c *ClusterReader) Next() (*Frame, error) {
	for {
		if len(c.pending) > 0 {
			fr := c.pending[0]
			c.pending = c.pending[1:]
			return fr, nil
		}
		if c.inCluster && c.clusterEnd != unknownSize && c.pos >= c.clusterEnd {
			c.inCluster = false
		}
		elemStart := c.pos
		id, size, err := c.readHeader()
		if err != nil {
			return nil, err
		}

		if c.inCluster && c.clusterEnd == unknownSize && isTopLevel(id) {
			c.inCluster = false
		}
		if !c.inCluster {
			if id != idCluster {
				if size == unknownSize {
					return nil, fmt.Errorf("mkv: unknown-size element 0x%X at %d", id, elemStart)
				}
				if err := c.skip(size); err != nil {
					return nil, err
				}
				continue
			}
			if c.StopAt != nil && (elemStart > c.StopAt.Cluster || (elemStart == c.StopAt.Cluster && c.StopAt.Rel == 0)) {
				return nil, io.EOF
			}
			c.inCluster = true
			c.clusterPos = elemStart
			c.clusterData = c.pos
			c.clusterTS = 0
			c.clusterEnd = unknownSize
			if size != unknownSize {
				c.clusterEnd = c.pos + size
			}
			continue
		}

		if size == unknownSize {
			return nil, fmt.Errorf("mkv: unknown-size child 0x%X in cluster at %d", id, c.clusterPos)
		}
		pos := Position{Cluster: c.clusterPos, Rel: elemStart - c.clusterData}
		switch id {
		case idTimestamp:
			b, err := c.readBody(size)
			if err != nil {
				return nil, err
			}
			c.clusterTS = int64(readUint(b))
		case idSimpleBlock, idBlockGroup:
			if c.StopAt != nil && !pos.Less(*c.StopAt) {
				return nil, io.EOF
			}
			if id == idSimpleBlock && !c.wantBlock(size) {
				if err := c.skip(size); err != nil {
					return nil, err
				}
				continue
			}
			b, err := c.readBody(size)
			if err != nil {
				return nil, err
			}
			if err := c.parseBlockElement(id, b, pos); err != nil {
				return nil, err
			}
		default:
			if err := c.skip(size); err != nil {
				return nil, err
			}
		}
	}
}

// wantBlock peeks at a SimpleBlock's track number to skip unwanted tracks
// without reading their data.
func (c *ClusterReader) wantBlock(size int64) bool {
	if c.Tracks == nil {
		return true
	}
	peek := 8
	if size < int64(peek) {
		peek = int(size)
	}
	b, err := c.br.Peek(peek)
	if err != nil {
		return true
	}
	track, _, err := parseSize(b)
	if err != nil {
		return true
	}
	return c.Tracks[uint64(track)]
}

func (c *ClusterReader) parseBlockElement(id uint32, b []byte, pos Position) error {
	if id == idSimpleBlock {
		return c.parseBlock(b, pos, -1, false)
	}
	var block []byte
	duration := int64(-1)
	hasRef := false
	if err := children(b, func(e element) error {
		switch e.id {
		case idBlock:
			block = e.data
		case idBlockDuration:
			duration = int64(readUint(e.data))
		case idReferenceBlock:
			hasRef = true
		}
		return nil
	}); err != nil {
		return err
	}
	if block == nil {
		return nil
	}
	return c.parseBlock(block, pos, duration, !hasRef)
}

// parseBlock decodes a (Simple)Block body. For a Block inside a BlockGroup,
// keyframe comes from the absence of ReferenceBlock.
func (c *ClusterReader) parseBlock(b []byte, pos Position, duration int64, groupKeyframe bool) error {
	trackNum, n, err := parseSize(b)
	if err != nil || trackNum < 0 {
		return fmt.Errorf("mkv: bad block track number at %+v", pos)
	}
	if c.Tracks != nil && !c.Tracks[uint64(trackNum)] {
		return nil
	}
	track := c.f.Track(uint64(trackNum))
	if track == nil {
		return nil
	}
	if len(b) < n+3 {
		return fmt.Errorf("mkv: truncated block at %+v", pos)
	}
	rel := int64(int16(binary.BigEndian.Uint16(b[n:])))
	flags := b[n+2]
	payload := b[n+3:]
	keyframe := groupKeyframe
	if duration < 0 {
		keyframe = flags&simpleBlockKeyframe != 0
	}

	laces, err := unlace(payload, (flags>>1)&3)
	if err != nil {
		return fmt.Errorf("mkv: block at %+v: %w", pos, err)
	}
	scale := c.f.TimestampScale
	pts := time.Duration((c.clusterTS + rel) * scale)
	var dur time.Duration
	if duration > 0 {
		dur = time.Duration(duration * scale)
	}
	for i, data := range laces {
		data, err := track.decodeFrame(data)
		if err != nil {
			return fmt.Errorf("mkv: decode frame of track %d: %w", trackNum, err)
		}
		fr := &Frame{
			Track:    uint64(trackNum),
			PTS:      pts + time.Duration(i)*track.DefaultDuration,
			Keyframe: keyframe,
			Data:     data,
			Pos:      pos,
		}
		if len(laces) == 1 {
			fr.Duration = dur
		}
		c.pending = append(c.pending, fr)
	}
	return nil
}

// unlace splits a laced block payload into frames.
func unlace(p []byte, lacing byte) ([][]byte, error) {
	if lacing == lacingNone {
		return [][]byte{p}, nil
	}
	if len(p) < 1 {
		return nil, errors.New("truncated lace header")
	}
	count := int(p[0]) + 1
	p = p[1:]
	sizes := make([]int64, count)
	switch lacing {
	case lacingXiph:
		for i := 0; i < count-1; i++ {
			for {
				if len(p) == 0 {
					return nil, errors.New("truncated Xiph lace")
				}
				v := p[0]
				p = p[1:]
				sizes[i] += int64(v)
				if v != 0xFF {
					break
				}
			}
		}
	case lacingEBML:
		first, n, err := parseSize(p)
		if err != nil || first < 0 {
			return nil, errors.New("bad EBML lace")
		}
		p = p[n:]
		sizes[0] = first
		for i := 1; i < count-1; i++ {
			raw, n, err := parseSize(p)
			if err != nil || raw < 0 {
				return nil, errors.New("bad EBML lace")
			}
			p = p[n:]
			// Signed vint: subtract the bias 2^(7n-1)-1.
			delta := raw - (int64(1)<<(7*n-1) - 1)
			sizes[i] = sizes[i-1] + delta
		}
	case lacingFixed:
		if len(p)%count != 0 {
			return nil, errors.New("fixed lace size mismatch")
		}
		for i := range sizes {
			sizes[i] = int64(len(p) / count)
		}
	}
	if lacing != lacingFixed {
		var sum int64
		for _, s := range sizes[:count-1] {
			if s < 0 {
				return nil, errors.New("negative lace size")
			}
			sum += s
		}
		if sum > int64(len(p)) {
			return nil, errors.New("lace sizes overrun block")
		}
		sizes[count-1] = int64(len(p)) - sum
	}
	frames := make([][]byte, count)
	for i, s := range sizes {
		frames[i] = p[:s]
		p = p[s:]
	}
	return frames, nil
}
