package sender

// MediaRemote (MRP) over the AirPlay 2 remote-control data stream: reads what
// an Apple TV is playing and sends it transport commands. Wire details are
// from pyatv (pyatv/protocols/airplay/ap2_session.py, channels.py and
// pyatv/protocols/mrp/protobuf; MIT); see docs/12-nowplaying.md.

import (
	"context"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"howett.net/plist"
)

const (
	dataStreamSalt       = "DataStream-Salt" // the stream seed is appended
	dataStreamOutputInfo = "DataStream-Output-Encryption-Key"
	dataStreamInputInfo  = "DataStream-Input-Encryption-Key"

	// dataStreamClientType is the clientTypeUUID pyatv announces for its MRP
	// tunnel.
	dataStreamClientType = "1910A70F-DBC0-4242-AF95-115DB30604E1"
	dataStreamType       = 130
	dataStreamControl    = 2
	// mrpSourceVersion is the sourceVersion pyatv announces for the tunnel.
	mrpSourceVersion = "550.10"

	dataFrameHeaderSize = 32
	dataFrameTagSync    = "sync"
	dataFrameTagReply   = "rply"
	dataFrameCommand    = "comm"
	dataFrameMaxSize    = 4 << 20
	hapFrameMaxPlain    = 1024

	mrpReplyTimeout = 5 * time.Second
)

// MRP ProtocolMessage.Type values and the extension field each message lives
// in (pyatv protobuf definitions).
const (
	mrpTypeSendCommand        = 1
	mrpTypeSetState           = 4
	mrpTypeDeviceInfo         = 15
	mrpTypeClientUpdates      = 16
	mrpTypeSetConnectionState = 38

	mrpFieldType       = 1
	mrpFieldIdentifier = 2
	mrpFieldErrorCode  = 4

	mrpFieldSendCommand        = 6
	mrpFieldSetState           = 9
	mrpFieldDeviceInfo         = 20
	mrpFieldClientUpdates      = 21
	mrpFieldSetConnectionState = 42

	mrpConnectionStateConnected = 2

	// MRPCommandPlay and friends are the MRP Command enum values.
	MRPCommandPlay            = 1
	MRPCommandPause           = 2
	MRPCommandTogglePlayPause = 3
)

// NowPlaying is what the Apple TV reports about its current playback.
type NowPlaying struct {
	// BundleID and App identify the app playing (com.google.ios.youtube).
	BundleID string
	App      string
	// State is "playing", "paused", "stopped", "interrupted", "seeking" or "".
	State string
	Title string
	// Artist is the artist, or a video's channel.
	Artist string
	// ContentID is the item's contentIdentifier, if the app sets one.
	ContentID string
	// Duration and Elapsed are in seconds. Elapsed is as of ElapsedAt.
	Duration, Elapsed float64
	// ElapsedAt is when Elapsed was true (Apple reference-date seconds, as
	// sent), or zero.
	ElapsedAt float64
	Rate      float64
}

// appleEpochOffset converts Apple reference-date seconds (2001-01-01) to Unix.
const appleEpochOffset = 978307200

// Position returns the playback position at now: Elapsed, advanced by the time
// since ElapsedAt while playing.
func (n NowPlaying) Position(now time.Time) float64 {
	pos := n.Elapsed
	if n.State == "playing" && n.Rate > 0 && n.ElapsedAt > 0 {
		at := time.Unix(int64(n.ElapsedAt)+appleEpochOffset, int64(math.Mod(n.ElapsedAt, 1)*1e9))
		if age := now.Sub(at).Seconds(); age > 0 && age < n.Duration+60 {
			pos += age * n.Rate
		}
	}
	if n.Duration > 0 && pos > n.Duration {
		pos = n.Duration
	}
	return pos
}

// merge overlays the fields u carries onto n. SET_STATE messages are partial:
// one sets the item, another only the playback state.
func (n NowPlaying) merge(u NowPlaying) NowPlaying {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	setF := func(dst *float64, v float64) {
		if v != 0 {
			*dst = v
		}
	}
	set(&n.BundleID, u.BundleID)
	set(&n.App, u.App)
	set(&n.State, u.State)
	set(&n.Title, u.Title)
	set(&n.Artist, u.Artist)
	set(&n.ContentID, u.ContentID)
	setF(&n.Duration, u.Duration)
	setF(&n.Elapsed, u.Elapsed)
	setF(&n.ElapsedAt, u.ElapsedAt)
	setF(&n.Rate, u.Rate)
	return n
}

// RemoteControl is an MRP session with a receiver. It is created by
// AirPlayClient.OpenRemoteControl.
type RemoteControl struct {
	conn net.Conn

	readAEAD, writeAEAD cipher.AEAD
	readNonce           uint64
	writeNonce          uint64
	readBuf             []byte
	writeMu             sync.Mutex
	seq                 uint64

	mu      sync.Mutex
	pending map[string]chan []byte
	// messages receives every incoming ProtocolMessage that is not the reply
	// to a request.
	messages chan []byte
	err      error
	done     chan struct{}
	event    net.Conn

	// players holds the merged state per app; current is the app whose state
	// changed last while it had a title. changed is closed (and replaced) on
	// every update.
	players map[string]NowPlaying
	current string
	changed chan struct{}
}

// OpenRemoteControl opens the remote-control data stream on a paired client
// and completes the MRP handshake. The client must have completed
// pair-verify.
func (c *AirPlayClient) OpenRemoteControl(ctx context.Context) (*RemoteControl, error) {
	if c.PairKeys == nil || len(c.PairKeys.SharedSecret) == 0 || !c.encrypted {
		return nil, errors.New("remote control needs an encrypted (paired) connection")
	}
	s := &PlaybackSession{
		client:      c,
		sessionURI:  fmt.Sprintf("rtsp://%s/%d", c.localIP(), mathrand.Uint32()),
		dacpHeaders: newDACPHeaders(),
	}
	resp, err := s.setup(map[string]interface{}{
		"isRemoteControlOnly": true,
		"osName":              playbackOSName,
		"sourceVersion":       mrpSourceVersion,
		"timingProtocol":      "None",
		"sessionUUID":         strings.ToUpper(generateUUID()),
		"name":                pairingClientName(),
	})
	if err != nil {
		return nil, err
	}
	r := &RemoteControl{
		pending:  make(map[string]chan []byte),
		messages: make(chan []byte, 64),
		done:     make(chan struct{}),
		players:  make(map[string]NowPlaying),
		changed:  make(chan struct{}),
	}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	// The receiver connects nothing; the sender dials its event port.
	if port := plistInt(resp["eventPort"]); port > 0 {
		if r.event, err = c.connectEventChannelWithHandler(ctx, port, nil, func(map[string]interface{}) {}); err != nil {
			return nil, err
		}
	}
	if _, _, err := c.rtspRequest("RECORD", s.sessionURI, "", nil, s.headers(map[string]string{
		"Range":    "npt=0-",
		"RTP-Info": "seq=0;rtptime=0",
	})); err != nil {
		return nil, fmt.Errorf("RECORD: %w", err)
	}

	seed := mathrand.Uint64()
	resp, err = s.setup(map[string]interface{}{
		"streams": []interface{}{map[string]interface{}{
			"controlType":          int64(dataStreamControl),
			"channelID":            strings.ToUpper(generateUUID()),
			"seed":                 seed,
			"clientUUID":           strings.ToUpper(generateUUID()),
			"type":                 int64(dataStreamType),
			"wantsDedicatedSocket": true,
			"clientTypeUUID":       dataStreamClientType,
		}},
	})
	if err != nil {
		return nil, err
	}
	streams, _ := resp["streams"].([]interface{})
	if len(streams) == 0 {
		return nil, errors.New("remote control SETUP returned no stream")
	}
	stream, _ := streams[0].(map[string]interface{})
	port := plistInt(stream["dataPort"])
	if port <= 0 {
		return nil, errors.New("remote control SETUP returned no data port")
	}

	var d net.Dialer
	r.conn, err = d.DialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("connect data stream: %w", err)
	}
	salt := []byte(dataStreamSalt + strconv.FormatUint(seed, 10))
	secret := c.PairKeys.SharedSecret
	if r.writeAEAD, err = chacha20poly1305.New(hkdfSHA512(secret, salt, []byte(dataStreamOutputInfo), 32)); err != nil {
		return nil, err
	}
	if r.readAEAD, err = chacha20poly1305.New(hkdfSHA512(secret, salt, []byte(dataStreamInputInfo), 32)); err != nil {
		return nil, err
	}
	r.seq = 0x100000000 + mathrand.Uint64()%0xFFFFFFFF
	go r.readLoop()

	if err := r.handshake(ctx, c.PairingID); err != nil {
		return nil, err
	}
	ok = true
	return r, nil
}

func (r *RemoteControl) handshake(ctx context.Context, pairingID string) error {
	info := pbMessage{
		{1, pbString(strings.ToUpper(pairingID))},
		{2, pbString(pairingClientName())},
		{3, pbString("iPhone")},
		{5, pbString("com.apple.TVRemote")},
		{6, pbString("344.28")},
		{7, pbVarint(1)},
		{8, pbVarint(108)},
		{9, pbVarint(1)},
		{10, pbVarint(1)},
		{12, pbString("com.apple.TVMusic")},
		{13, pbVarint(1)},
		{14, pbVarint(1)},
		{15, pbVarint(1)},
		{17, pbVarint(2)},
		{21, pbVarint(3)}, // DeviceClass.iPhone
		{22, pbVarint(1)},
	}
	if _, err := r.request(ctx, mrpTypeDeviceInfo, mrpFieldDeviceInfo, info); err != nil {
		return fmt.Errorf("MRP device info: %w", err)
	}
	if err := r.send(mrpTypeSetConnectionState, mrpFieldSetConnectionState, pbMessage{
		{1, pbVarint(mrpConnectionStateConnected)},
	}); err != nil {
		return err
	}
	if err := r.send(mrpTypeClientUpdates, mrpFieldClientUpdates, pbMessage{
		// pyatv's defaults: everything but now-playing, which it learns from
		// the pushed SET_STATE messages.
		{1, pbVarint(1)}, // artwork
		{2, pbVarint(0)}, // now playing
		{3, pbVarint(1)}, // volume
		{4, pbVarint(1)}, // keyboard
		{5, pbVarint(1)}, // output devices
	}); err != nil {
		return fmt.Errorf("MRP client updates: %w", err)
	}
	return nil
}

// Command sends a transport command (MRPCommandPause, ...).
func (r *RemoteControl) Command(ctx context.Context, command int) error {
	reply, err := r.request(ctx, mrpTypeSendCommand, mrpFieldSendCommand, pbMessage{{1, pbVarint(uint64(command))}})
	if err != nil {
		return err
	}
	if code := pbParse(reply).varint(mrpFieldErrorCode); code != 0 {
		return fmt.Errorf("MRP command %d: error code %d", command, code)
	}
	return nil
}

// Messages returns the raw ProtocolMessages the receiver pushes (SET_STATE and
// others), for ParseNowPlaying and for dumping.
func (r *RemoteControl) Messages() <-chan []byte { return r.messages }

// Done is closed when the connection ends; Err then says why.
func (r *RemoteControl) Done() <-chan struct{} { return r.done }

// Err returns the error that ended the session.
func (r *RemoteControl) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close ends the session.
func (r *RemoteControl) Close() {
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if r.event != nil {
		_ = r.event.Close()
	}
}

func newMRPIdentifier() string { return strings.ToUpper(generateUUID()) }

func (r *RemoteControl) frame(msgType int, field int, body pbMessage, id string) []byte {
	m := pbMessage{
		{mrpFieldType, pbVarint(uint64(msgType))},
		{mrpFieldIdentifier, pbString(id)},
		{field, pbBytes(body.encode())},
	}
	return m.encode()
}

func (r *RemoteControl) send(msgType, field int, body pbMessage) error {
	return r.writeProtobuf(r.frame(msgType, field, body, newMRPIdentifier()))
}

func (r *RemoteControl) request(ctx context.Context, msgType, field int, body pbMessage) ([]byte, error) {
	id := newMRPIdentifier()
	ch := make(chan []byte, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
	}()
	if err := r.writeProtobuf(r.frame(msgType, field, body, id)); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-r.done:
		return nil, fmt.Errorf("connection closed: %w", r.Err())
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(mrpReplyTimeout):
		return nil, errors.New("no reply from the receiver")
	}
}

// writeProtobuf sends one protobuf as a sync/comm data frame.
func (r *RemoteControl) writeProtobuf(message []byte) error {
	data := binary.AppendUvarint(nil, uint64(len(message)))
	data = append(data, message...)
	payload, err := plist.Marshal(map[string]interface{}{
		"params": map[string]interface{}{"data": data},
	}, plist.BinaryFormat)
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.seq++
	return r.writeFrame(dataFrameTagSync, dataFrameCommand, r.seq, payload)
}

func (r *RemoteControl) writeFrame(tag, command string, seq uint64, payload []byte) error {
	frame := make([]byte, dataFrameHeaderSize, dataFrameHeaderSize+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(dataFrameHeaderSize+len(payload)))
	copy(frame[4:16], tag) // zero padded
	if command != "" {
		copy(frame[16:20], command)
	}
	binary.BigEndian.PutUint64(frame[20:], seq)
	frame = append(frame, payload...)

	var out []byte
	for len(frame) > 0 {
		chunk := frame[:min(len(frame), hapFrameMaxPlain)]
		frame = frame[len(chunk):]
		aad := binary.LittleEndian.AppendUint16(nil, uint16(len(chunk)))
		nonce := make([]byte, 12)
		binary.LittleEndian.PutUint64(nonce[4:], r.writeNonce)
		r.writeNonce++
		out = append(out, aad...)
		out = r.writeAEAD.Seal(out, nonce, chunk, aad)
	}
	_, err := r.conn.Write(out)
	return err
}

func (r *RemoteControl) readHAP() ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r.conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.LittleEndian.Uint16(lenBuf[:]))
	ct := make([]byte, n+r.readAEAD.Overhead())
	if _, err := io.ReadFull(r.conn, ct); err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	binary.LittleEndian.PutUint64(nonce[4:], r.readNonce)
	r.readNonce++
	return r.readAEAD.Open(nil, nonce, ct, lenBuf[:])
}

func (r *RemoteControl) readLoop() {
	err := r.readFrames()
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
	close(r.done)
	close(r.messages)
}

func (r *RemoteControl) readFrames() error {
	for {
		for len(r.readBuf) < dataFrameHeaderSize || len(r.readBuf) < int(binary.BigEndian.Uint32(r.readBuf)) {
			chunk, err := r.readHAP()
			if err != nil {
				return err
			}
			r.readBuf = append(r.readBuf, chunk...)
			if len(r.readBuf) >= 4 && binary.BigEndian.Uint32(r.readBuf) > dataFrameMaxSize {
				return fmt.Errorf("data frame of %d bytes", binary.BigEndian.Uint32(r.readBuf))
			}
		}
		size := int(binary.BigEndian.Uint32(r.readBuf))
		if size < dataFrameHeaderSize {
			return fmt.Errorf("data frame size %d", size)
		}
		frame := r.readBuf[:size]
		r.readBuf = append([]byte(nil), r.readBuf[size:]...)
		tag := strings.TrimRight(string(frame[4:16]), "\x00")
		seq := binary.BigEndian.Uint64(frame[20:])
		if tag == dataFrameTagSync {
			r.writeMu.Lock()
			err := r.writeFrame(dataFrameTagReply, "", seq, nil)
			r.writeMu.Unlock()
			if err != nil {
				return err
			}
		}
		payload := frame[dataFrameHeaderSize:]
		if len(payload) == 0 {
			continue
		}
		var body struct {
			Params struct {
				Data []byte `plist:"data"`
			} `plist:"params"`
		}
		if _, err := plist.Unmarshal(payload, &body); err != nil {
			dbg("[MRP] undecodable payload: %v", err)
			continue
		}
		data := body.Params.Data
		for len(data) > 0 {
			n, k := binary.Uvarint(data)
			if k <= 0 || uint64(len(data)-k) < n {
				dbg("[MRP] truncated protobuf")
				break
			}
			r.dispatch(append([]byte(nil), data[k:k+int(n)]...))
			data = data[k+int(n):]
		}
	}
}

// NowPlaying returns the most recently updated player that has a title, and
// whether there is one.
func (r *RemoteControl) NowPlaying() (NowPlaying, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	np, ok := r.players[r.current]
	return np, ok && np.Title != ""
}

// WaitState blocks until ok(NowPlaying) holds for the current player, the
// session ends, or ctx is done.
func (r *RemoteControl) WaitState(ctx context.Context, ok func(NowPlaying) bool) (NowPlaying, error) {
	for {
		r.mu.Lock()
		changed := r.changed
		np, have := r.players[r.current]
		r.mu.Unlock()
		if have && np.Title != "" && ok(np) {
			return np, nil
		}
		select {
		case <-changed:
		case <-r.done:
			return NowPlaying{}, fmt.Errorf("connection closed: %w", r.Err())
		case <-ctx.Done():
			return NowPlaying{}, ctx.Err()
		}
	}
}

func (r *RemoteControl) track(message []byte) {
	np, ok := ParseNowPlaying(message)
	if !ok {
		return
	}
	// A message without an app (rare) belongs to the current one.
	r.mu.Lock()
	defer r.mu.Unlock()
	key := np.BundleID
	if key == "" {
		key = r.current
	}
	merged := r.players[key].merge(np)
	r.players[key] = merged
	if merged.Title != "" {
		r.current = key
	}
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *RemoteControl) dispatch(message []byte) {
	r.track(message)
	dbg("[MRP] <- message type %d (%d bytes)", pbParse(message).varint(mrpFieldType), len(message))
	id := pbParse(message).str(mrpFieldIdentifier)
	r.mu.Lock()
	ch := r.pending[id]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- message:
		default:
		}
		return
	}
	select {
	case r.messages <- message:
	default:
		dbg("[MRP] dropping message; consumer is behind")
	}
}

// ParseNowPlaying extracts playback state from a SET_STATE message. ok is
// false for any other message type. Fields not in the message are left zero;
// merge into the previous value with Merge.
func ParseNowPlaying(message []byte) (np NowPlaying, ok bool) {
	pm := pbParse(message)
	if pm.varint(mrpFieldType) != mrpTypeSetState {
		return np, false
	}
	state := pbParse(pm.bytes(mrpFieldSetState))
	switch state.varint(6) {
	case 1:
		np.State = "playing"
	case 2:
		np.State = "paused"
	case 3:
		np.State = "stopped"
	case 4:
		np.State = "interrupted"
	case 5:
		np.State = "seeking"
	}
	np.BundleID = state.str(4)
	np.App = state.str(5)
	if client := pbParse(pbParse(state.bytes(9)).bytes(2)); len(client) > 0 {
		if np.BundleID == "" {
			np.BundleID = client.str(2)
		}
		if np.App == "" {
			np.App = client.str(7)
		}
	}
	info := pbParse(state.bytes(1))
	np.Title = info.str(9)
	np.Artist = info.str(2)
	np.Duration = info.double(3)
	np.Elapsed = info.double(4)
	np.Rate = float64(info.float(5))
	np.ElapsedAt = info.double(8)
	// The current item, if the message carries a queue, is more specific.
	queue := pbParse(state.bytes(3))
	loc := int(queue.varint(1))
	for i, item := range queue.all(2) {
		if i != loc {
			continue
		}
		md := pbParse(pbParse(item).bytes(2))
		if t := md.str(1); t != "" {
			np.Title = t
		}
		if a := md.str(7); a != "" {
			np.Artist = a
		}
		np.ContentID = md.str(44)
		if d := md.double(14); d > 0 {
			np.Duration = d
		}
		if e := md.double(35); e > 0 {
			np.Elapsed = e
		}
		if r := md.float(39); r != 0 {
			np.Rate = float64(r)
		}
		if t := md.double(74); t > 0 {
			np.ElapsedAt = t
		}
	}
	return np, true
}

// pbMessage is a protobuf message under construction: field number and value
// pairs, in order.
type pbMessage []struct {
	field int
	value pbValue
}

type pbValue struct {
	wire byte // 0 varint, 1 fixed64, 2 length-delimited, 5 fixed32
	n    uint64
	b    []byte
}

func pbVarint(n uint64) pbValue { return pbValue{wire: 0, n: n} }
func pbString(s string) pbValue { return pbValue{wire: 2, b: []byte(s)} }
func pbBytes(b []byte) pbValue  { return pbValue{wire: 2, b: b} }

func (m pbMessage) encode() []byte {
	var out []byte
	for _, f := range m {
		out = binary.AppendUvarint(out, uint64(f.field)<<3|uint64(f.value.wire))
		switch f.value.wire {
		case 0:
			out = binary.AppendUvarint(out, f.value.n)
		case 1:
			out = binary.LittleEndian.AppendUint64(out, f.value.n)
		case 5:
			out = binary.LittleEndian.AppendUint32(out, uint32(f.value.n))
		default:
			out = binary.AppendUvarint(out, uint64(len(f.value.b)))
			out = append(out, f.value.b...)
		}
	}
	return out
}

// pbFields is a decoded protobuf message.
type pbFields []pbField

type pbField struct {
	num  int
	wire byte
	n    uint64 // varint, fixed32 or fixed64 value
	b    []byte
}

// pbParse decodes the top-level fields of a message. Malformed trailing data
// is ignored.
func pbParse(data []byte) pbFields {
	var fields pbFields
	for len(data) > 0 {
		key, k := binary.Uvarint(data)
		if k <= 0 {
			break
		}
		data = data[k:]
		f := pbField{num: int(key >> 3), wire: byte(key & 7)}
		switch f.wire {
		case 0:
			f.n, k = binary.Uvarint(data)
			if k <= 0 {
				return fields
			}
			data = data[k:]
		case 1:
			if len(data) < 8 {
				return fields
			}
			f.n = binary.LittleEndian.Uint64(data)
			data = data[8:]
		case 5:
			if len(data) < 4 {
				return fields
			}
			f.n = uint64(binary.LittleEndian.Uint32(data))
			data = data[4:]
		case 2:
			n, k := binary.Uvarint(data)
			if k <= 0 || uint64(len(data)-k) < n {
				return fields
			}
			f.b = data[k : k+int(n)]
			data = data[k+int(n):]
		default:
			return fields
		}
		fields = append(fields, f)
	}
	return fields
}

func (p pbFields) find(num int) *pbField {
	for i := range p {
		if p[i].num == num {
			return &p[i]
		}
	}
	return nil
}

func (p pbFields) varint(num int) uint64 {
	if f := p.find(num); f != nil {
		return f.n
	}
	return 0
}

func (p pbFields) str(num int) string {
	if f := p.find(num); f != nil {
		return string(f.b)
	}
	return ""
}

func (p pbFields) bytes(num int) []byte {
	if f := p.find(num); f != nil {
		return f.b
	}
	return nil
}

func (p pbFields) all(num int) [][]byte {
	var out [][]byte
	for _, f := range p {
		if f.num == num {
			out = append(out, f.b)
		}
	}
	return out
}

func (p pbFields) double(num int) float64 {
	if f := p.find(num); f != nil {
		return math.Float64frombits(f.n)
	}
	return 0
}

func (p pbFields) float(num int) float32 {
	if f := p.find(num); f != nil {
		return math.Float32frombits(uint32(f.n))
	}
	return 0
}

// DumpProtobuf renders a message's fields for diagnostics, recursing into
// length-delimited fields that parse as messages.
func DumpProtobuf(message []byte) string {
	var sb strings.Builder
	dumpProtobuf(&sb, message, 0)
	return sb.String()
}

func dumpProtobuf(sb *strings.Builder, message []byte, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, f := range pbParse(message) {
		switch f.wire {
		case 0:
			fmt.Fprintf(sb, "%s%d: %d\n", indent, f.num, f.n)
		case 1:
			fmt.Fprintf(sb, "%s%d: %d (double %g)\n", indent, f.num, f.n, math.Float64frombits(f.n))
		case 5:
			fmt.Fprintf(sb, "%s%d: %d (float %g)\n", indent, f.num, f.n, math.Float32frombits(uint32(f.n)))
		case 2:
			if strings.HasPrefix(string(f.b), "bplist00") {
				var v interface{}
				if _, err := plist.Unmarshal(f.b, &v); err == nil {
					fmt.Fprintf(sb, "%s%d: plist %v\n", indent, f.num, v)
					continue
				}
			}
			if printable(f.b) {
				fmt.Fprintf(sb, "%s%d: %q\n", indent, f.num, f.b)
			} else if sub := pbParse(f.b); len(sub) > 0 && depth < 8 && len(f.b) < 1<<16 {
				fmt.Fprintf(sb, "%s%d {\n", indent, f.num)
				dumpProtobuf(sb, f.b, depth+1)
				fmt.Fprintf(sb, "%s}\n", indent)
			} else {
				fmt.Fprintf(sb, "%s%d: <%d bytes>\n", indent, f.num, len(f.b))
			}
		}
	}
}

func printable(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
