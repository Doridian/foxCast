// Package rtmp is a minimal RTMP ingest server: it accepts one publishing
// client per connection (such as OBS) and turns what it publishes into an FLV
// byte stream. It implements the simple handshake, the chunk stream, and the
// NetConnection/NetStream commands a publisher needs (connect, createStream,
// publish). See docs/11-rtmp-ingest.md.
package rtmp

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Protocol constants (Adobe RTMP specification 1.0, sections 5 and 7).
const (
	handshakeVersion = 3
	handshakeSize    = 1536

	defaultChunkSize = 128
	serverChunkSize  = 4096
	maxChunkSize     = 1 << 24
	maxMessageSize   = 16 << 20 // the 3-byte message length allows 16 MiB
	windowAckSize    = 2500000
	peerBandwidth    = 2500000
	peerBandwidthDyn = 2
	extendedStamp    = 0xffffff

	// Message type IDs.
	msgSetChunkSize     = 1
	msgAbort            = 2
	msgAck              = 3
	msgUserControl      = 4
	msgWindowAckSize    = 5
	msgSetPeerBandwidth = 6
	msgAudio            = 8
	msgVideo            = 9
	msgDataAMF3         = 15
	msgCommandAMF3      = 17
	msgDataAMF0         = 18
	msgCommandAMF0      = 20

	// User control event types.
	eventStreamBegin = 0
	eventPingRequest = 6
	eventPingReply   = 7

	// Chunk stream IDs used for what the server sends.
	csidControl = 2
	csidCommand = 3
	csidStream  = 5

	// publishStreamID is the message stream createStream hands out.
	publishStreamID = 1

	// FLV framing (FLV specification 10.1, annex E).
	flvTagHeaderSize = 11
	flvFlagsAudio    = 0x04
	flvFlagsVideo    = 0x01
)

// flvHeader is the FLV file header announcing audio and video, followed by
// the first (zero) previous-tag size.
var flvHeader = []byte{'F', 'L', 'V', 1, flvFlagsAudio | flvFlagsVideo, 0, 0, 0, 9, 0, 0, 0, 0}

// ErrNotPublishing is returned by Accept for a client that asks to play
// rather than publish.
var ErrNotPublishing = errors.New("rtmp: client did not publish")

// Publisher is a client publishing a stream. Read returns the stream as FLV:
// a header, then one tag per audio or video message. It returns io.EOF when
// the client unpublishes or disconnects.
type Publisher struct {
	// App is the application the client connected to ("live" in
	// rtmp://host/live) and Key the stream name it publishes.
	App, Key string

	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration

	chunkSize  uint32
	streams    map[uint32]*chunkStream
	writeChunk uint32

	ackWindow uint32 // the client's window: acknowledge each this many bytes
	received  uint32
	acked     uint32

	pending []byte // FLV bytes not yet returned by Read
	started bool   // the FLV header has been queued
	ended   bool
}

// chunkStream is the header state of one chunk stream ID.
type chunkStream struct {
	timestamp uint32 // of the current message, absolute
	delta     uint32
	length    uint32
	typeID    uint8
	streamID  uint32
	extended  bool // the last header used an extended timestamp
	payload   []byte
}

// message is a complete RTMP message.
type message struct {
	typeID    uint8
	streamID  uint32
	timestamp uint32
	payload   []byte
}

// Accept performs the handshake and command exchange on conn until the
// client starts publishing. timeout bounds each read, both here and in
// Read, so a client that goes silent is dropped. On error conn is closed.
func Accept(conn net.Conn, timeout time.Duration) (*Publisher, error) {
	p := &Publisher{
		conn:       conn,
		r:          bufio.NewReaderSize(conn, 64<<10),
		timeout:    timeout,
		chunkSize:  defaultChunkSize,
		streams:    map[uint32]*chunkStream{},
		writeChunk: defaultChunkSize,
	}
	if err := p.handshake(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rtmp handshake: %w", err)
	}
	if err := p.negotiate(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return p, nil
}

// Close disconnects the client.
func (p *Publisher) Close() error {
	return p.conn.Close()
}

// Read implements io.Reader over the published FLV stream.
func (p *Publisher) Read(b []byte) (int, error) {
	if !p.started {
		p.started = true
		p.pending = append(p.pending, flvHeader...)
	}
	for len(p.pending) == 0 {
		if p.ended {
			return 0, io.EOF
		}
		msg, err := p.readMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return 0, io.EOF
			}
			return 0, err
		}
		if err := p.handleMedia(msg); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	if len(p.pending) == 0 {
		p.pending = p.pending[:0:0]
	}
	return n, nil
}

// handleMedia queues audio and video as FLV tags and answers the commands a
// publishing client may still send.
func (p *Publisher) handleMedia(msg message) error {
	switch msg.typeID {
	case msgAudio, msgVideo:
		if len(msg.payload) == 0 {
			return nil
		}
		p.pending = appendFLVTag(p.pending, msg.typeID, msg.timestamp, msg.payload)
	case msgCommandAMF0, msgCommandAMF3:
		name, _, _, err := parseCommand(msg)
		if err != nil {
			return err
		}
		switch name {
		case "FCUnpublish", "deleteStream", "closeStream":
			p.ended = true
		}
	}
	// Metadata (@setDataFrame) is dropped: the decoder learns everything it
	// needs from the codec configuration in the first audio and video tags.
	return nil
}

// appendFLVTag appends one FLV tag and its previous-tag size.
func appendFLVTag(b []byte, typeID uint8, timestamp uint32, payload []byte) []byte {
	size := uint32(len(payload))
	b = append(b, typeID, byte(size>>16), byte(size>>8), byte(size),
		byte(timestamp>>16), byte(timestamp>>8), byte(timestamp), byte(timestamp>>24),
		0, 0, 0)
	b = append(b, payload...)
	return binary.BigEndian.AppendUint32(b, size+flvTagHeaderSize)
}

// handshake runs the simple (unsigned) handshake. Clients such as librtmp,
// which OBS uses, fall back to it when S1 carries a zero version field.
func (p *Publisher) handshake() error {
	p.deadline()
	c0c1 := make([]byte, 1+handshakeSize)
	if _, err := io.ReadFull(p.r, c0c1); err != nil {
		return err
	}
	if c0c1[0] != handshakeVersion {
		return fmt.Errorf("unsupported version %d", c0c1[0])
	}
	s := make([]byte, 1+2*handshakeSize)
	s[0] = handshakeVersion
	s1 := s[1 : 1+handshakeSize]
	binary.BigEndian.PutUint32(s1, uint32(time.Now().Unix()))
	if _, err := rand.Read(s1[8:]); err != nil {
		return err
	}
	copy(s[1+handshakeSize:], c0c1[1:]) // S2 echoes C1
	if _, err := p.conn.Write(s); err != nil {
		return err
	}
	c2 := make([]byte, handshakeSize)
	_, err := io.ReadFull(p.r, c2)
	return err
}

// negotiate answers commands until the client publishes.
func (p *Publisher) negotiate() error {
	for {
		msg, err := p.readMessage()
		if err != nil {
			return fmt.Errorf("rtmp: %w", err)
		}
		if msg.typeID != msgCommandAMF0 && msg.typeID != msgCommandAMF3 {
			continue
		}
		name, txn, args, err := parseCommand(msg)
		if err != nil {
			return err
		}
		switch name {
		case "connect":
			if len(args) > 0 {
				if obj, ok := args[0].(map[string]any); ok {
					p.App, _ = obj["app"].(string)
				}
			}
			if err := p.acceptConnect(txn); err != nil {
				return err
			}
		case "createStream":
			if err := p.sendCommand(csidCommand, 0, "_result", txn, nil, publishStreamID); err != nil {
				return err
			}
		case "publish":
			if len(args) > 1 {
				p.Key, _ = args[1].(string)
			}
			return p.startPublish()
		case "play", "play2":
			return ErrNotPublishing
		}
		// releaseStream, FCPublish and the like need no answer.
	}
}

func (p *Publisher) acceptConnect(txn float64) error {
	if err := p.sendControl(msgWindowAckSize, binary.BigEndian.AppendUint32(nil, windowAckSize)); err != nil {
		return err
	}
	bw := binary.BigEndian.AppendUint32(nil, peerBandwidth)
	if err := p.sendControl(msgSetPeerBandwidth, append(bw, peerBandwidthDyn)); err != nil {
		return err
	}
	if err := p.sendControl(msgSetChunkSize, binary.BigEndian.AppendUint32(nil, serverChunkSize)); err != nil {
		return err
	}
	p.writeChunk = serverChunkSize
	return p.sendCommand(csidCommand, 0, "_result", txn,
		object{{"fmsVer", "FMS/3,0,1,123"}, {"capabilities", 31}},
		object{
			{"level", "status"},
			{"code", "NetConnection.Connect.Success"},
			{"description", "Connection succeeded."},
			{"objectEncoding", 0},
		})
}

func (p *Publisher) startPublish() error {
	begin := binary.BigEndian.AppendUint16(nil, eventStreamBegin)
	begin = binary.BigEndian.AppendUint32(begin, publishStreamID)
	if err := p.sendControl(msgUserControl, begin); err != nil {
		return err
	}
	return p.sendCommand(csidStream, publishStreamID, "onStatus", 0, nil, object{
		{"level", "status"},
		{"code", "NetStream.Publish.Start"},
		{"description", "Publishing " + p.Key + "."},
		{"details", p.Key},
	})
}

// parseCommand splits a command message into its name, transaction ID and
// remaining arguments (the command object, usually null, comes first).
func parseCommand(msg message) (string, float64, []any, error) {
	payload := msg.payload
	if msg.typeID == msgCommandAMF3 && len(payload) > 0 {
		payload = payload[1:] // AMF3 commands still carry AMF0 values after a format byte
	}
	values, err := decodeAMF0(payload)
	if err != nil {
		return "", 0, nil, fmt.Errorf("rtmp: command: %w", err)
	}
	if len(values) < 2 {
		return "", 0, nil, errors.New("rtmp: command without a name and transaction")
	}
	name, ok := values[0].(string)
	if !ok {
		return "", 0, nil, errors.New("rtmp: command name is not a string")
	}
	txn, _ := values[1].(float64)
	return name, txn, values[2:], nil
}

// deadline extends the connection's read and write deadline.
func (p *Publisher) deadline() {
	if p.timeout > 0 {
		_ = p.conn.SetDeadline(time.Now().Add(p.timeout))
	}
}

// readMessage reads chunks until a message completes, handling protocol
// control messages itself.
func (p *Publisher) readMessage() (message, error) {
	for {
		msg, ok, err := p.readChunk()
		if err != nil {
			return message{}, err
		}
		if !ok {
			continue
		}
		switch msg.typeID {
		case msgSetChunkSize:
			if len(msg.payload) < 4 {
				return message{}, errors.New("short set chunk size")
			}
			size := binary.BigEndian.Uint32(msg.payload) & 0x7fffffff
			if size == 0 || size > maxChunkSize {
				return message{}, fmt.Errorf("invalid chunk size %d", size)
			}
			p.chunkSize = size
		case msgAbort:
			if len(msg.payload) >= 4 {
				if cs := p.streams[binary.BigEndian.Uint32(msg.payload)]; cs != nil {
					cs.payload = nil
				}
			}
		case msgWindowAckSize:
			if len(msg.payload) >= 4 {
				p.ackWindow = binary.BigEndian.Uint32(msg.payload)
			}
		case msgUserControl:
			if len(msg.payload) >= 6 && binary.BigEndian.Uint16(msg.payload) == eventPingRequest {
				reply := binary.BigEndian.AppendUint16(nil, eventPingReply)
				if err := p.sendControl(msgUserControl, append(reply, msg.payload[2:6]...)); err != nil {
					return message{}, err
				}
			}
		case msgAck, msgSetPeerBandwidth:
		default:
			return msg, nil
		}
	}
}

// readChunk reads one chunk. It returns a message when the chunk completes
// one.
func (p *Publisher) readChunk() (message, bool, error) {
	p.deadline()
	first, err := p.readByte()
	if err != nil {
		return message{}, false, err
	}
	format := first >> 6
	csid := uint32(first & 0x3f)
	switch csid {
	case 0:
		b, err := p.readN(1)
		if err != nil {
			return message{}, false, err
		}
		csid = 64 + uint32(b[0])
	case 1:
		b, err := p.readN(2)
		if err != nil {
			return message{}, false, err
		}
		csid = 64 + uint32(b[0]) + uint32(b[1])<<8
	}
	cs := p.streams[csid]
	if cs == nil {
		if format != 0 {
			return message{}, false, fmt.Errorf("chunk stream %d starts with a type %d header", csid, format)
		}
		cs = &chunkStream{}
		p.streams[csid] = cs
	}
	newMessage := cs.payload == nil

	var stamp uint32
	switch format {
	case 0, 1, 2:
		size := [3]int{11, 7, 3}[format]
		h, err := p.readN(size)
		if err != nil {
			return message{}, false, err
		}
		stamp = uint32(h[0])<<16 | uint32(h[1])<<8 | uint32(h[2])
		if format <= 1 {
			cs.length = uint32(h[3])<<16 | uint32(h[4])<<8 | uint32(h[5])
			cs.typeID = h[6]
		}
		if format == 0 {
			cs.streamID = binary.LittleEndian.Uint32(h[7:11])
		}
		cs.extended = stamp == extendedStamp
	}
	if cs.extended {
		b, err := p.readN(4)
		if err != nil {
			return message{}, false, err
		}
		if format != 3 {
			stamp = binary.BigEndian.Uint32(b)
		}
	}
	if newMessage {
		switch format {
		case 0:
			cs.timestamp, cs.delta = stamp, 0
		case 1, 2:
			cs.delta = stamp
			cs.timestamp += stamp
		case 3:
			cs.timestamp += cs.delta
		}
		if cs.length > maxMessageSize {
			return message{}, false, fmt.Errorf("message of %d bytes", cs.length)
		}
		cs.payload = make([]byte, 0, cs.length)
	}

	n := min(p.chunkSize, cs.length-uint32(len(cs.payload)))
	data, err := p.readN(int(n))
	if err != nil {
		return message{}, false, err
	}
	cs.payload = append(cs.payload, data...)
	if uint32(len(cs.payload)) < cs.length {
		return message{}, false, nil
	}
	msg := message{typeID: cs.typeID, streamID: cs.streamID, timestamp: cs.timestamp, payload: cs.payload}
	cs.payload = nil
	return msg, true, p.acknowledge()
}

func (p *Publisher) readByte() (byte, error) {
	b, err := p.r.ReadByte()
	if err == nil {
		p.received++
	}
	return b, err
}

func (p *Publisher) readN(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(p.r, b); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = io.EOF
		}
		return nil, err
	}
	p.received += uint32(n)
	return b, nil
}

// acknowledge sends an acknowledgement once the client's window has been
// received since the last one.
func (p *Publisher) acknowledge() error {
	if p.ackWindow == 0 || p.received-p.acked < p.ackWindow {
		return nil
	}
	p.acked = p.received
	return p.sendControl(msgAck, binary.BigEndian.AppendUint32(nil, p.received))
}

func (p *Publisher) sendControl(typeID uint8, payload []byte) error {
	return p.send(csidControl, message{typeID: typeID, payload: payload})
}

func (p *Publisher) sendCommand(csid, streamID uint32, name string, txn float64, values ...any) error {
	payload := encodeAMF0(nil, name, txn)
	payload = encodeAMF0(payload, values...)
	return p.send(csid, message{typeID: msgCommandAMF0, streamID: streamID, payload: payload})
}

// send writes a message as one type 0 chunk followed by type 3
// continuations. The server's messages all carry timestamp 0.
func (p *Publisher) send(csid uint32, msg message) error {
	size := len(msg.payload)
	b := []byte{byte(csid), 0, 0, 0, byte(size >> 16), byte(size >> 8), byte(size), msg.typeID}
	b = binary.LittleEndian.AppendUint32(b, msg.streamID)
	for i := 0; ; {
		n := min(int(p.writeChunk), size-i)
		b = append(b, msg.payload[i:i+n]...)
		i += n
		if i >= size {
			break
		}
		b = append(b, 0xc0|byte(csid))
	}
	p.deadline()
	_, err := p.conn.Write(b)
	return err
}
