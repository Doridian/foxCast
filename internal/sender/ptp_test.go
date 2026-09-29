package sender

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"net"
	"net/netip"
	"testing"
	"time"
)

// Captured from a HomePod mini (AirTunes 980.77.2) following this sender.
const (
	homePodFollowUpHex  = "18020060000004080000016b46600000000000000499b9b7e0f10008800c000102fd00000011e86133fcf9540003001c0080c20000010000000000000000000000000000000000000000000000030010000d930000040499b9b7e0f100080000"
	homePodDelayRespHex = "19020036000006080000028d212c0000000000000499b9b7e0f100088010000100fd00000011e8e7162925d802adeffffee5edc30001"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePTPHeaderHomePodFollowUp(t *testing.T) {
	packet := mustHex(t, homePodFollowUpHex)
	header, err := parsePTPHeader(packet)
	if err != nil {
		t.Fatal(err)
	}
	want := ptpHeader{
		messageType: ptpMessageFollowUp,
		correction:  23807584 * time.Nanosecond,
		clockID:     0x0499b9b7e0f10008,
		portNumber:  32780,
		sequenceID:  1,
	}
	if header != want {
		t.Fatalf("header = %+v, want %+v", header, want)
	}
	if got, want := parsePTPTimestamp(packet[ptpHeaderLength:]), 1173601*time.Second+872216916*time.Nanosecond; got != want {
		t.Fatalf("preciseOriginTimestamp = %v, want %v", got, want)
	}
}

func TestParsePTPHeaderHomePodDelayResp(t *testing.T) {
	packet := mustHex(t, homePodDelayRespHex)
	header, err := parsePTPHeader(packet)
	if err != nil {
		t.Fatal(err)
	}
	if header.messageType != ptpMessageDelayResp || header.sequenceID != 1 || len(packet) != ptpDelayRespLength {
		t.Fatalf("header = %+v (len %d), want Delay_Resp seq 1 len %d", header, len(packet), ptpDelayRespLength)
	}
	if got := binary.BigEndian.Uint64(packet[ptpHeaderLength+ptpTimestampLength:]); got != 0x02adeffffee5edc3 {
		t.Fatalf("requestingPortIdentity clock = 0x%016x", got)
	}
}

func TestParsePTPHeaderRejectsBadMessages(t *testing.T) {
	packet := mustHex(t, homePodFollowUpHex)
	for name, bad := range map[string][]byte{
		"short":     packet[:ptpHeaderLength-1],
		"version 1": append([]byte{packet[0], 0x01}, packet[2:]...),
		"truncated": packet[:len(packet)-1],
	} {
		if _, err := parsePTPHeader(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestPTPDelayRequest(t *testing.T) {
	got := ptpDelayRequest(0x02adeffffee5edc3, 7)
	want := mustHex(t, "11"+"02"+"002c"+"00"+"00"+"0400"+ // Delay_Req, 802.1AS, v2, 44 bytes, unicast
		"0000000000000000"+"00000000"+ // correction, reserved
		"02adeffffee5edc3"+"0001"+"0007"+ // sourcePortIdentity, sequenceId
		"01"+"7f"+ // controlField, logMessageInterval
		"00000000000000000000") // originTimestamp
	if !bytes.Equal(got, want) {
		t.Fatalf("Delay_Req = %x, want %x", got, want)
	}
}

// syntheticReceiver is a receiver clock running rate fast against local
// time, and a network whose transit is floor plus a deterministic jitter
// that reaches down to the floor now and then, as queueing does.
type syntheticReceiver struct {
	base   time.Time
	origin time.Duration
	rate   float64
	floor  time.Duration
}

func (r syntheticReceiver) at(local time.Time) time.Duration {
	elapsed := local.Sub(r.base)
	return r.origin + elapsed + time.Duration(float64(elapsed)*r.rate)
}

func (r syntheticReceiver) transit(i int) time.Duration {
	jitter := time.Duration((i*7919)%97) * 100 * time.Microsecond
	if i%5 == 0 {
		jitter = 0
	}
	return r.floor + jitter
}

func TestPTPSyncEstimatorFollowsRateAndLowerEnvelope(t *testing.T) {
	receiver := syntheticReceiver{base: time.Unix(1000, 0), origin: 1173601 * time.Second, rate: 80e-6, floor: 2 * time.Millisecond}
	var e ptpSyncEstimator
	var local time.Time
	var remote time.Duration
	var rate float64
	var rated bool
	for i := 0; i < 8*90; i++ {
		sent := receiver.base.Add(time.Duration(i) * 125 * time.Millisecond)
		local, remote, rate, rated = e.add(sent.Add(receiver.transit(i)), receiver.at(sent))
	}
	if !rated || math.Abs(rate-receiver.rate) > 5e-6 {
		t.Fatalf("rate = %+.1f ppm (rated %v), want %+.1f ppm", rate*1e6, rated, receiver.rate*1e6)
	}
	// The estimate is the receiver's time less the minimum transit.
	want := receiver.at(local) - receiver.floor
	if diff := remote - want; diff < -200*time.Microsecond || diff > 200*time.Microsecond {
		t.Fatalf("estimate off by %v", diff)
	}
}

func TestPTPSyncEstimatorWaitsForARateSpan(t *testing.T) {
	var e ptpSyncEstimator
	base := time.Unix(1000, 0)
	for i := 0; i < 8; i++ {
		at := base.Add(time.Duration(i) * 125 * time.Millisecond)
		if _, _, _, rated := e.add(at, time.Duration(i)*125*time.Millisecond); rated {
			t.Fatalf("rated after %d Syncs spanning %v", i+1, time.Duration(i)*125*time.Millisecond)
		}
	}
}

func ptpTestMessage(messageType uint8, seq uint16, timestamp time.Duration, correction time.Duration) []byte {
	length := ptpHeaderLength + ptpTimestampLength
	packet := make([]byte, length)
	packet[0] = ptpTransportSpecific | messageType
	packet[1] = ptpVersion
	binary.BigEndian.PutUint16(packet[2:4], uint16(length))
	binary.BigEndian.PutUint64(packet[8:16], uint64(correction)<<16)
	binary.BigEndian.PutUint64(packet[20:28], 0x0499b9b7e0f10008)
	binary.BigEndian.PutUint16(packet[30:32], seq)
	seconds := uint64(timestamp / time.Second)
	binary.BigEndian.PutUint16(packet[34:36], uint16(seconds>>32))
	binary.BigEndian.PutUint32(packet[36:40], uint32(seconds))
	binary.BigEndian.PutUint32(packet[40:44], uint32(timestamp%time.Second))
	return packet
}

func TestPTPListenerSteersClockFromSyncs(t *testing.T) {
	receiver := syntheticReceiver{base: time.Unix(1000, 0), origin: 1173601 * time.Second, rate: -30e-6, floor: 3 * time.Millisecond}
	clock := &mediaClock{timelineID: 0x0499b9b7e0f10008}
	peerAddr := netip.MustParseAddr("192.0.2.7")
	l := &PTPListener{peers: map[netip.Addr]*ptpPeer{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.follow(ctx, peerAddr, clock)
	peer := l.peers[peerAddr]
	// A symmetric 3 ms path: 6 ms round trips.
	for i := 0; i < ptpSettleRoundTrips; i++ {
		sent := receiver.base.Add(time.Duration(i) * time.Second)
		peer.addDelayRequest(uint16(i), sent)
		peer.completeDelayRequest(uint16(i), sent.Add(2*receiver.floor))
	}

	for i := 0; i < 8*20; i++ {
		sent := receiver.base.Add(time.Duration(i) * 125 * time.Millisecond)
		// Split the origin between the preciseOriginTimestamp and the
		// correction field, and deliver every other Follow_Up first.
		origin := receiver.at(sent)
		followUp := ptpTestMessage(ptpMessageFollowUp, uint16(i), origin-time.Millisecond, time.Millisecond)
		sync := ptpTestMessage(ptpMessageSync, uint16(i), 0, 0)
		arrival := sent.Add(receiver.transit(i))
		if i%2 == 0 {
			l.handle(peerAddr, followUp, arrival.Add(time.Millisecond))
			l.handle(peerAddr, sync, arrival)
		} else {
			l.handle(peerAddr, sync, arrival)
			l.handle(peerAddr, followUp, arrival.Add(time.Millisecond))
		}
	}
	if len(peer.syncs) != 0 {
		t.Fatalf("%d Syncs left unpaired", len(peer.syncs))
	}
	local := receiver.base.Add(20 * time.Second)
	got := clock.remoteAtLocked(local)
	if diff := got - receiver.at(local); diff < -300*time.Microsecond || diff > 300*time.Microsecond {
		t.Fatalf("clock off by %v after 20 s of Syncs", diff)
	}
	if clock.ptpSampledAt.IsZero() {
		t.Fatal("clock did not record the PTP sample")
	}

	// Messages from anyone else are ignored.
	l.handle(netip.MustParseAddr("192.0.2.8"), ptpTestMessage(ptpMessageSync, 999, 0, 0), local)
	if _, ok := peer.syncs[999]; ok {
		t.Fatal("stranger's Sync was taken")
	}
}

func TestPTPListenerTimesDelayRequestRoundTrips(t *testing.T) {
	master, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	l, err := listenPTP(0, 0, master.LocalAddr().(*net.UDPAddr).Port, 10*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.follow(ctx, netip.MustParseAddr("127.0.0.1"), &mediaClock{timelineID: 1})
	general := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: l.general.LocalAddr().(*net.UDPAddr).Port}

	// Answer Delay_Reqs as the HomePod does, to the general port.
	buf := make([]byte, 128)
	for answered := 0; answered < 3; answered++ {
		_ = master.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := master.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("waiting for Delay_Req: %v", err)
		}
		request, err := parsePTPHeader(buf[:n])
		if err != nil || request.messageType != ptpMessageDelayReq || buf[0]&0xf0 != ptpTransportSpecific {
			t.Fatalf("got %x, want a Delay_Req with transportSpecific 1", buf[:n])
		}
		response := make([]byte, ptpDelayRespLength)
		copy(response, ptpTestMessage(ptpMessageDelayResp, request.sequenceID, 0, 0))
		binary.BigEndian.PutUint16(response[2:4], ptpDelayRespLength)
		binary.BigEndian.PutUint64(response[ptpHeaderLength+ptpTimestampLength:], request.clockID)
		binary.BigEndian.PutUint16(response[ptpHeaderLength+ptpTimestampLength+8:], request.portNumber)
		if _, err := master.WriteToUDP(response, general); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		trips := len(l.peers[netip.MustParseAddr("127.0.0.1")].roundTrips)
		l.mu.Unlock()
		if trips >= 3 {
			if _, ok := l.Stats(netip.MustParseAddr("127.0.0.1")); !ok {
				t.Fatal("no stats after round trips")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Delay_Resps were not matched to their requests")
}

func TestMediaClockSwitchesToPTPWhenHeadersAreOnAnotherClock(t *testing.T) {
	start := time.Unix(1000, 0)
	clock := &mediaClock{anchorLocal: start, anchorRemote: 5000 * time.Second, timelineID: 1}
	// The receiver's PTP time is 16 minutes behind its clock headers.
	ptpTime := 5000*time.Second - 16*time.Minute
	local := start.Add(time.Second)
	clock.observePTP(local, ptpTime+time.Second, 0, false)
	if got := clock.remoteAtLocked(local); got != ptpTime+time.Second {
		t.Fatalf("clock = %v after the first PTP sample, want %v", got, ptpTime+time.Second)
	}
	select {
	case <-clock.ptpStarted:
	default:
		t.Fatal("first PTP sample did not signal waitForPTP")
	}

	// From then on, clock headers do not steer it back.
	sent := local.Add(time.Second)
	if err := clock.observe(map[string]string{
		"x-apple-requestreceivedtimestamp": "5002000",
		"x-apple-processingtime":           "0",
	}, requestTimes{sent: sent, received: sent}); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.remoteAtLocked(sent), ptpTime+2*time.Second; got != want {
		t.Fatalf("clock = %v after a header sample, want %v", got, want)
	}
}

func TestMediaClockSlewsSmallPTPTakeover(t *testing.T) {
	start := time.Unix(1000, 0)
	clock := &mediaClock{anchorLocal: start, anchorRemote: 5000 * time.Second, timelineID: 1}
	local := start.Add(time.Second)
	// 3 ms behind the header estimate: an estimation error, not another
	// clock, so the timeline must not step back.
	clock.observePTP(local, 5001*time.Second-3*time.Millisecond, 0, false)
	if got := clock.remoteAtLocked(local); got != 5001*time.Second {
		t.Fatalf("clock stepped to %v", got)
	}
}

func TestMediaClockWaitForPTPTimesOut(t *testing.T) {
	clock := &mediaClock{timelineID: 1}
	if clock.waitForPTP(context.Background(), 10*time.Millisecond) {
		t.Fatal("waitForPTP reported PTP without a sample")
	}
	go clock.observePTP(time.Now(), time.Second, 0, false)
	if !clock.waitForPTP(context.Background(), 2*time.Second) {
		t.Fatal("waitForPTP missed the first sample")
	}
}

func TestPTPPeerStats(t *testing.T) {
	ms := time.Millisecond
	peer := &ptpPeer{roundTrips: []time.Duration{4 * ms, 6 * ms, 5 * ms, 9 * ms}, samples: ptpSettleSyncs}
	st := peer.stats()
	if !st.Locked {
		t.Error("peer with enough samples and round trips is not locked")
	}
	if st.Latency != 6*ms {
		t.Errorf("latency = %v, want the median 6ms", st.Latency)
	}
	// |6-4| + |5-6| + |9-5| = 7ms over 3 changes.
	if want := 7 * ms / 3; st.Jitter != want {
		t.Errorf("jitter = %v, want %v", st.Jitter, want)
	}
	peer.samples = 1
	if peer.stats().Locked {
		t.Error("unsettled peer is locked")
	}
}
