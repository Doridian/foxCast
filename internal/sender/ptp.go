package sender

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// PTP follower for receivers that time sessions with PTP. Observed on HomePod
// mini (AirTunes 980.77.2, 2026-09-28): once a SETUP lists this sender in
// timingPeerInfo, the receiver (usually its own grandmaster) sends IEEE 802.1AS
// flavoured PTPv2 unicast over UDP to the listed address: two-step Sync on
// 319 (8/s), and Follow_Up (with the 802.1AS Follow_Up information TLV),
// Announce and Signaling on 320. It answers a Delay_Req that carries
// transportSpecific 1 with a Delay_Resp. That Delay_Resp's receiveTimestamp
// sits on the receiver's Sync schedule rather than at the request's arrival,
// so it is not used; the response's arrival still times the round trip.
// docs/04-audio-streaming.md has the details.

const (
	ptpEventPort   = 319
	ptpGeneralPort = 320

	ptpVersion = 2
	// ptpTransportSpecific (majorSdoId 1, IEEE 802.1AS) goes in the first
	// header byte. HomePods ignore a Delay_Req without it.
	ptpTransportSpecific = 0x10

	ptpMessageSync      = 0x0
	ptpMessageDelayReq  = 0x1
	ptpMessageFollowUp  = 0x8
	ptpMessageDelayResp = 0x9
	ptpMessageAnnounce  = 0xb

	ptpHeaderLength       = 34
	ptpTimestampLength    = 10
	ptpDelayReqLength     = ptpHeaderLength + ptpTimestampLength
	ptpDelayRespLength    = ptpHeaderLength + ptpTimestampLength + 10
	ptpAnnounceGMOffset   = 53
	ptpFlagUnicast        = 0x0400
	ptpControlDelayReq    = 1
	ptpLogIntervalUnknown = 0x7f

	// ptpSyncWindow is how many Syncs the rate estimate draws on: 60 s at
	// the HomePod's 8/s. NTP slewing of the local clock changes its rate over
	// minutes.
	ptpSyncWindow = 480
	// ptpSyncBin is how many Syncs share one lower-envelope point: 1 s at
	// 8/s.
	ptpSyncBin = 8
	// ptpOffsetBins is how many of the latest lower-envelope points the time
	// estimate averages.
	ptpOffsetBins = 8
	// ptpMinRateSpan is the least time the lower-envelope points must span
	// for a rate estimate.
	ptpMinRateSpan = 4 * time.Second
	// ptpMaxRate bounds the rate estimate; kernel NTP slewing stays within
	// 500 ppm.
	ptpMaxRate = 1000e-6
	// ptpRoundTripWindow is how many Delay_Req round trips (one a second)
	// the path delay estimate draws on. Wi-Fi congestion can hold every
	// round trip of a few seconds tens of milliseconds above the floor.
	ptpRoundTripWindow = 64
	// ptpSettleSyncs and ptpSettleRoundTrips are what a peer needs before
	// its estimate steers the clock: one lower-envelope bin and a few round
	// trips. The rate follows once the bins span ptpMinRateSpan.
	ptpSettleSyncs      = ptpSyncBin
	ptpSettleRoundTrips = 3
	// Delay_Reqs go every ptpDelayReqInterval, and every
	// ptpStartDelayReqInterval for a peer's first ptpStartDelayReqs, so a
	// session gets its path delay quickly.
	ptpDelayReqInterval      = time.Second
	ptpStartDelayReqInterval = 250 * time.Millisecond
	ptpStartDelayReqs        = 8
	// ptpStartTimeout is how long a speaker session waits for the
	// receiver's PTP clock before it starts on the clock headers.
	ptpStartTimeout = 3 * time.Second
	// ptpPendingLimit bounds unanswered Delay_Reqs and unpaired Syncs.
	ptpPendingLimit = 16
	// ptpLogEvery thins the per-Sync debug log.
	ptpLogEvery = 32
	// ptpStatsWindow is how many of the latest round trips PTPStats covers.
	ptpStatsWindow = 16
)

// PTPListener follows the PTP clocks of receivers in PTP sessions over UDP
// 319 and 320. One listener serves every session of the process. Binding
// those ports needs CAP_NET_BIND_SERVICE or a lowered
// net.ipv4.ip_unprivileged_port_start; without a listener, PTP sessions are
// timed from the receivers' RTSP clock headers.
type PTPListener struct {
	event, general *net.UDPConn
	peerEventPort  int
	clockID        uint64

	mu       sync.Mutex
	peers    map[netip.Addr]*ptpPeer
	delaySeq uint16

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ListenPTP binds the PTP ports and starts following receivers as sessions
// register them. Close releases the ports.
func ListenPTP() (*PTPListener, error) {
	return listenPTP(ptpEventPort, ptpGeneralPort, ptpEventPort, ptpStartDelayReqInterval, ptpDelayReqInterval)
}

// listenPTP binds eventPort and generalPort (0 picks free ports) and sends
// Delay_Reqs to receivers' peerEventPort, every startInterval for a peer's
// first ptpStartDelayReqs and every interval after.
func listenPTP(eventPort, generalPort, peerEventPort int, startInterval, interval time.Duration) (*PTPListener, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("generate PTP clock identity: %w", err)
	}
	event, err := net.ListenUDP("udp4", &net.UDPAddr{Port: eventPort})
	if err != nil {
		return nil, fmt.Errorf("bind PTP event port: %w", err)
	}
	general, err := net.ListenUDP("udp4", &net.UDPAddr{Port: generalPort})
	if err != nil {
		event.Close()
		return nil, fmt.Errorf("bind PTP general port: %w", err)
	}
	for _, conn := range []*net.UDPConn{event, general} {
		if err := enablePacketTimestamps(conn); err != nil {
			dbg("[PTP] kernel receive timestamps unavailable (%v); using read time", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &PTPListener{
		event:         event,
		general:       general,
		peerEventPort: peerEventPort,
		clockID:       binary.BigEndian.Uint64(id[:]),
		peers:         make(map[netip.Addr]*ptpPeer),
		cancel:        cancel,
	}
	l.wg.Add(3)
	go l.readLoop(event)
	go l.readLoop(general)
	go l.delayLoop(ctx, startInterval, interval)
	return l, nil
}

// Close stops following all receivers and releases the ports.
func (l *PTPListener) Close() error {
	l.cancel()
	err := errors.Join(l.event.Close(), l.general.Close())
	l.wg.Wait()
	return err
}

// follow feeds clock from the PTP messages of the receiver at addr until ctx
// ends.
func (l *PTPListener) follow(ctx context.Context, addr netip.Addr, clock *mediaClock) {
	addr = addr.Unmap()
	peer := &ptpPeer{
		addr:          addr,
		clock:         clock,
		syncs:         make(map[uint16]ptpPendingSync),
		delayRequests: make(map[uint16]time.Time),
	}
	l.mu.Lock()
	l.peers[addr] = peer
	l.mu.Unlock()
	dbg("[PTP] following %s", addr)
	go func() {
		<-ctx.Done()
		l.mu.Lock()
		if l.peers[addr] == peer {
			delete(l.peers, addr)
		}
		l.mu.Unlock()
	}()
}

// PTPStats describes how well a followed receiver's clock is being tracked.
type PTPStats struct {
	// Locked is set once PTP, rather than the RTSP clock headers, times the
	// session.
	Locked bool
	// Latency is the median of the latest Delay_Req round trips.
	Latency time.Duration
	// Jitter is the mean change between consecutive round trips of those.
	Jitter time.Duration
	// LastSync is when the latest Sync arrived; zero before the first.
	LastSync time.Time
}

// Stats reports on the receiver at addr. ok is false while it is not
// followed or no round trip has completed yet.
func (l *PTPListener) Stats(addr netip.Addr) (stats PTPStats, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	peer := l.peers[addr.Unmap()]
	if peer == nil || len(peer.roundTrips) == 0 {
		return PTPStats{}, false
	}
	return peer.stats(), true
}

func (l *PTPListener) readLoop(conn *net.UDPConn) {
	defer l.wg.Done()
	buf := make([]byte, 1500)
	oob := make([]byte, 128)
	for {
		n, oobn, _, from, err := conn.ReadMsgUDPAddrPort(buf, oob)
		now := time.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			dbg("[PTP] read: %v", err)
			continue
		}
		l.handle(from.Addr().Unmap(), buf[:n], packetTime(oob[:oobn], now))
	}
}

func (l *PTPListener) delayLoop(ctx context.Context, startInterval, interval time.Duration) {
	defer l.wg.Done()
	ticker := time.NewTicker(startInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		l.mu.Lock()
		for addr, peer := range l.peers {
			due := interval
			if peer.delayRequestsSent < ptpStartDelayReqs {
				due = startInterval
			}
			if time.Since(peer.lastDelayRequest) < due-startInterval/2 {
				continue
			}
			l.delaySeq++
			seq := l.delaySeq
			sent := time.Now()
			if _, err := l.event.WriteToUDPAddrPort(ptpDelayRequest(l.clockID, seq), netip.AddrPortFrom(addr, uint16(l.peerEventPort))); err != nil {
				dbg("[PTP] Delay_Req to %s: %v", addr, err)
				continue
			}
			peer.addDelayRequest(seq, sent)
			peer.delayRequestsSent++
			peer.lastDelayRequest = sent
		}
		l.mu.Unlock()
	}
}

func (l *PTPListener) handle(from netip.Addr, packet []byte, at time.Time) {
	header, err := parsePTPHeader(packet)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	peer := l.peers[from]
	if peer == nil {
		return
	}
	switch header.messageType {
	case ptpMessageSync:
		peer.pairSync(header.sequenceID, ptpPendingSync{arrival: at})
	case ptpMessageFollowUp:
		if len(packet) < ptpHeaderLength+ptpTimestampLength {
			return
		}
		origin := parsePTPTimestamp(packet[ptpHeaderLength:]) + header.correction
		peer.pairSync(header.sequenceID, ptpPendingSync{origin: origin, followUp: true})
	case ptpMessageDelayResp:
		if len(packet) < ptpDelayRespLength ||
			binary.BigEndian.Uint64(packet[ptpHeaderLength+ptpTimestampLength:]) != l.clockID {
			return
		}
		peer.completeDelayRequest(header.sequenceID, at)
	case ptpMessageAnnounce:
		if len(packet) >= ptpAnnounceGMOffset+8 {
			peer.announce(binary.BigEndian.Uint64(packet[ptpAnnounceGMOffset:]))
		}
	}
}

// ptpPeer is the state of one followed receiver. PTPListener.mu guards it.
type ptpPeer struct {
	addr          netip.Addr
	clock         *mediaClock
	syncs         map[uint16]ptpPendingSync
	estimator     ptpSyncEstimator
	delayRequests map[uint16]time.Time
	// delayRequestsSent and lastDelayRequest pace the Delay_Reqs.
	delayRequestsSent int
	lastDelayRequest  time.Time
	roundTrips        []time.Duration
	grandmaster       uint64
	samples           int
	lastSync          time.Time
}

// ptpPendingSync is half of a two-step Sync: the Sync's arrival or its
// Follow_Up's origin time, whichever came first (they use different ports).
type ptpPendingSync struct {
	arrival  time.Time
	origin   time.Duration
	followUp bool
}

func (p *ptpPeer) pairSync(seq uint16, half ptpPendingSync) {
	other, ok := p.syncs[seq]
	if !ok || other.followUp == half.followUp {
		if len(p.syncs) >= ptpPendingLimit {
			clear(p.syncs)
		}
		p.syncs[seq] = half
		return
	}
	delete(p.syncs, seq)
	arrival, origin := half.arrival, other.origin
	if half.followUp {
		arrival, origin = other.arrival, half.origin
	}
	local, remote, rate, rated := p.estimator.add(arrival, origin)
	p.samples++
	p.lastSync = arrival
	if !p.settled() {
		// Until then the receiver's clock headers keep timing the session.
		return
	}
	oneWay := p.oneWayDelay()
	correction := p.clock.observePTP(local, remote+oneWay, rate, rated)
	if p.samples%ptpLogEvery == 1 {
		// The offset from wall time compares receivers of one group.
		dbg("[PTP] %s sync sample %d: one-way=%v rate=%+.1fppm correction=%v wall-offset=%v",
			p.addr, p.samples, oneWay, rate*1e6, correction, remote+oneWay-time.Duration(local.UnixNano()))
	}
}

// settled reports whether PTP samples time the session yet.
func (p *ptpPeer) settled() bool {
	return p.samples >= ptpSettleSyncs && len(p.roundTrips) >= ptpSettleRoundTrips
}

func (p *ptpPeer) stats() PTPStats {
	recent := p.roundTrips[max(0, len(p.roundTrips)-ptpStatsWindow):]
	var jitter time.Duration
	for i := 1; i < len(recent); i++ {
		d := recent[i] - recent[i-1]
		jitter += max(d, -d)
	}
	if len(recent) > 1 {
		jitter /= time.Duration(len(recent) - 1)
	}
	sorted := slices.Clone(recent)
	slices.Sort(sorted)
	return PTPStats{
		Locked:   p.settled(),
		Latency:  sorted[len(sorted)/2],
		Jitter:   jitter,
		LastSync: p.lastSync,
	}
}

func (p *ptpPeer) addDelayRequest(seq uint16, sent time.Time) {
	if len(p.delayRequests) >= ptpPendingLimit {
		clear(p.delayRequests)
	}
	p.delayRequests[seq] = sent
}

func (p *ptpPeer) completeDelayRequest(seq uint16, arrival time.Time) {
	sent, ok := p.delayRequests[seq]
	if !ok {
		return
	}
	delete(p.delayRequests, seq)
	if len(p.roundTrips) == ptpRoundTripWindow {
		copy(p.roundTrips, p.roundTrips[1:])
		p.roundTrips = p.roundTrips[:len(p.roundTrips)-1]
	}
	p.roundTrips = append(p.roundTrips, arrival.Sub(sent))
}

// oneWayDelay estimates the receiver-to-sender delay as half the fastest
// recent round trip, taking the path to be symmetric. The round trip
// includes the receiver's time to answer, so this errs long. Without round
// trips it is zero.
func (p *ptpPeer) oneWayDelay() time.Duration {
	if len(p.roundTrips) == 0 {
		return 0
	}
	fastest := p.roundTrips[0]
	for _, rtt := range p.roundTrips[1:] {
		if rtt < fastest {
			fastest = rtt
		}
	}
	return fastest / 2
}

func (p *ptpPeer) announce(grandmaster uint64) {
	if grandmaster != p.grandmaster {
		p.grandmaster = grandmaster
		dbg("[PTP] %s grandmaster 0x%016x", p.addr, grandmaster)
	}
}

// ptpSyncEstimator follows the lower envelope of Sync transit. In each bin of
// ptpSyncBin Syncs, the one that arrived soonest after its origin time crossed
// the network with the least queueing. A least-squares line through those
// minima over the whole window gives the receiver clock's rate against the
// local clock, and the mean of the latest ptpOffsetBins minima at that rate
// gives its time less the minimum one-way delay. Keeping the rate to the long
// window and the offset to the recent bins stops rate noise from swinging the
// offset. Minima are picked again against the first line, and nothing
// carries over between calls, so errors cannot build up.
//
// On a congested Wi-Fi link to a HomePod (round trips 6-74 ms, 2026-09-28),
// replaying two minutes of Syncs put this estimate within about ±0.6 ms of a
// fit over all of them, where the rate-free lower envelope was off by up to
// 7 ms.
type ptpSyncEstimator struct {
	samples []ptpSyncSample
}

type ptpSyncSample struct {
	arrival time.Time
	origin  time.Duration
}

// add records a Sync and returns the receiver's estimated time, less the
// one-way delay, at its arrival, and its rate against the local clock (the
// fraction by which it runs fast). rated says the window spans enough time
// for a rate; otherwise rate is zero and should not replace a previous one.
func (e *ptpSyncEstimator) add(arrival time.Time, origin time.Duration) (local time.Time, remote time.Duration, rate float64, rated bool) {
	if len(e.samples) == ptpSyncWindow {
		copy(e.samples, e.samples[1:])
		e.samples = e.samples[:len(e.samples)-1]
	}
	e.samples = append(e.samples, ptpSyncSample{arrival: arrival, origin: origin})

	// Work in offsets from the first sample, where float64 is exact enough:
	// x is local time, y is how far the receiver's clock has moved beyond it.
	first := e.samples[0]
	point := func(s ptpSyncSample) (x, y float64) {
		elapsed := s.arrival.Sub(first.arrival)
		return float64(elapsed), float64(s.origin - first.origin - elapsed)
	}
	// minima are the least-transit samples of each bin, newest bin first,
	// judged against a line of slope rate: the furthest above it.
	minima := func(rate float64) (xs, ys []float64) {
		for end := len(e.samples); end > 0; end -= ptpSyncBin {
			bin := e.samples[max(0, end-ptpSyncBin):end]
			bestX, bestY := point(bin[0])
			for _, s := range bin[1:] {
				if x, y := point(s); y-rate*x > bestY-rate*bestX {
					bestX, bestY = x, y
				}
			}
			xs, ys = append(xs, bestX), append(ys, bestY)
		}
		return xs, ys
	}
	for pass := 0; pass < 2; pass++ {
		xs, ys := minima(rate)
		if len(xs) < 3 || time.Duration(xs[0]-xs[len(xs)-1]) < ptpMinRateSpan {
			break
		}
		rate, _ = fitLine(xs, ys)
		rate = math.Max(-ptpMaxRate, math.Min(ptpMaxRate, rate))
		rated = true
	}
	xs, ys := minima(rate)
	var offset float64
	n := min(len(xs), ptpOffsetBins)
	for k := 0; k < n; k++ {
		offset += (ys[k] - rate*xs[k]) / float64(n)
	}
	x, _ := point(e.samples[len(e.samples)-1])
	remote = first.origin + time.Duration(x+offset+rate*x)
	return arrival, remote, rate, rated
}

// fitLine is the least-squares line y = intercept + slope*x.
func fitLine(xs, ys []float64) (slope, intercept float64) {
	n := float64(len(xs))
	var meanX, meanY float64
	for i := range xs {
		meanX += xs[i] / n
		meanY += ys[i] / n
	}
	var sxx, sxy float64
	for i := range xs {
		dx := xs[i] - meanX
		sxx += dx * dx
		sxy += dx * (ys[i] - meanY)
	}
	slope = sxy / sxx
	return slope, meanY - slope*meanX
}

type ptpHeader struct {
	messageType uint8
	correction  time.Duration
	clockID     uint64
	portNumber  uint16
	sequenceID  uint16
}

func parsePTPHeader(packet []byte) (ptpHeader, error) {
	if len(packet) < ptpHeaderLength {
		return ptpHeader{}, fmt.Errorf("PTP message is %d bytes, shorter than its header", len(packet))
	}
	if version := packet[1] & 0x0f; version != ptpVersion {
		return ptpHeader{}, fmt.Errorf("PTP version %d", version)
	}
	if length := int(binary.BigEndian.Uint16(packet[2:4])); length > len(packet) {
		return ptpHeader{}, fmt.Errorf("PTP message says %d bytes, got %d", length, len(packet))
	}
	return ptpHeader{
		messageType: packet[0] & 0x0f,
		// correctionField is in nanoseconds scaled by 2^16.
		correction: time.Duration(int64(binary.BigEndian.Uint64(packet[8:16])) >> 16),
		clockID:    binary.BigEndian.Uint64(packet[20:28]),
		portNumber: binary.BigEndian.Uint16(packet[28:30]),
		sequenceID: binary.BigEndian.Uint16(packet[30:32]),
	}, nil
}

// parsePTPTimestamp reads a PTP timestamp: 48-bit seconds, 32-bit nanoseconds.
func parsePTPTimestamp(b []byte) time.Duration {
	seconds := uint64(binary.BigEndian.Uint16(b[0:2]))<<32 | uint64(binary.BigEndian.Uint32(b[2:6]))
	return time.Duration(seconds)*time.Second + time.Duration(binary.BigEndian.Uint32(b[6:10]))
}

func ptpDelayRequest(clockID uint64, seq uint16) []byte {
	packet := make([]byte, ptpDelayReqLength)
	packet[0] = ptpTransportSpecific | ptpMessageDelayReq
	packet[1] = ptpVersion
	binary.BigEndian.PutUint16(packet[2:4], ptpDelayReqLength)
	binary.BigEndian.PutUint16(packet[6:8], ptpFlagUnicast)
	binary.BigEndian.PutUint64(packet[20:28], clockID)
	binary.BigEndian.PutUint16(packet[28:30], 1)
	binary.BigEndian.PutUint16(packet[30:32], seq)
	packet[32] = ptpControlDelayReq
	packet[33] = ptpLogIntervalUnknown
	return packet
}
