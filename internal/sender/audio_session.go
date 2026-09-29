package sender

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"time"
)

// SetupAudioOnly negotiates an AirPlay 2 realtime audio session with no
// screen stream, for receivers used as speakers (HomePod, AirPlay 2 speakers,
// or a TV when only its sound is wanted). The session is sequenced like
// pyatv's audio sender: a session SETUP with NTP timing, RECORD, then one
// type 96 stream with isMedia set. Receivers that reject the control-only
// SETUP get the media-first form once, as screen mirroring does.
//
// The returned session streams with StreamAudio and ends with Close; it has
// no video, so StreamFrames must not be used. cfg.NoAudio, FPS and the video
// fields are ignored. The receiver's volume is left where it is.
func (c *AirPlayClient) SetupAudioOnly(ctx context.Context, cfg StreamConfig) (*MirrorSession, error) {
	policy, err := compatibilityForReceiver(c.info, c.encrypted, false)
	if err != nil {
		return nil, err
	}
	codec, err := speakerAudioCodec(c.info)
	if err != nil {
		return nil, fmt.Errorf("negotiate speaker audio: %w", err)
	}
	sessionUUID := generateUUID()
	setupRequest := mirrorSetupRequest{
		deviceID:      uuidToMAC(c.sessionID),
		sessionUUID:   sessionUUID,
		sourceVersion: policy.sourceVersion(),
		// pyatv streams to AirPlay 2 speakers with NTP timing, including
		// receivers that also offer PTP.
		timingProtocol: timingProtocolNTP,
		name:           pairingClientName(),
		audioOnly:      true,
	}

	ports, err := allocateConsecutiveUDPPortsInRange(3, cfg.PortMin, cfg.PortMax)
	if err != nil {
		return nil, fmt.Errorf("allocate audio ports: %w", err)
	}
	timingConn, ctrlConn, dataConn := ports[0], ports[1], ports[2]
	sessionCtx, cancelSession := context.WithCancel(ctx)
	var eventConn net.Conn
	setupSucceeded := false
	defer func() {
		if setupSucceeded {
			return
		}
		cancelSession()
		if eventConn != nil {
			eventConn.Close()
		}
		for _, conn := range ports {
			conn.Close()
		}
	}()
	setupRequest.timingPort = timingConn.LocalAddr().(*net.UDPAddr).Port
	go ntpTimingResponder(sessionCtx, timingConn)

	streamConnectionID := int64(time.Now().UnixNano() & 0x7FFFFFFFFFFFFFFF)
	uri := fmt.Sprintf("rtsp://%s:%d/%d", c.host, c.port, streamConnectionID)
	var chachaKey []byte
	if policy.audioSecurity == audioSecurityChaCha {
		if chachaKey, err = generateAudioChaChaKey(rand.Reader); err != nil {
			return nil, fmt.Errorf("generate ChaCha20-Poly1305 audio key: %w", err)
		}
	}
	var aesKey, aesIV []byte
	if policy.audioSecurity == audioSecurityLegacyAES && c.fpKey != nil && c.fpIV != nil {
		aesKey, aesIV = c.fpKey, c.fpIV
	}

	eventPort := 0
	timingProbesStarted := false
	observe := func(response map[string]interface{}) error {
		if port := plistInt(response["timingPort"]); port > 0 && !timingProbesStarted {
			timingProbesStarted = true
			go sendNTPTimingProbes(sessionCtx, timingConn, c.host, port)
		}
		if port := plistInt(response["eventPort"]); port > 0 {
			eventPort = port
		}
		if eventConn != nil || eventPort == 0 {
			return nil
		}
		var err error
		if eventConn, err = c.connectEventChannel(sessionCtx, eventPort, nil); err != nil {
			return fmt.Errorf("connect receiver event channel: %w", err)
		}
		return nil
	}
	record := func(response map[string]interface{}) error {
		if skip, _ := response["skipRecord"].(bool); skip {
			dbg("[SETUP] receiver returned skipRecord=true; session was started by SETUP")
			return nil
		}
		headers := map[string]string{
			"Session":  sessionUUID,
			"Range":    "npt=0-",
			"RTP-Info": "seq=0;rtptime=0",
		}
		if _, _, err := c.rtspRequest("RECORD", uri, "", nil, headers); err != nil {
			return fmt.Errorf("RECORD: %w", err)
		}
		return nil
	}

	dbg("[SETUP] speaker phase 1 (control): preparing audio session")
	controlPlist := setupRequest.sessionPlist()
	if policy.fairPlayOnControl() {
		addFairPlayRootFields(controlPlist, c.FpEkey, c.fpIV, true)
	}
	sessionFirst := true
	controlResp, _, _, err := c.requestSetup(uri, "control", controlPlist)
	if err != nil {
		if !setupOrderRejected(err) {
			return nil, err
		}
		sessionFirst = false
		dbg("[SETUP] receiver rejected control-first SETUP; negotiating media-first speaker SETUP")
	} else {
		if err := observe(controlResp); err != nil {
			return nil, err
		}
		if err := record(controlResp); err != nil {
			return nil, err
		}
	}

	latencySamples := speakerLatencySamples()
	ct, spf, audioFormat, _, _, _ := codec.Info()
	streamBase := map[string]interface{}{
		"type":               int64(96),
		"streamConnectionID": streamConnectionID,
		"ct":                 ct,
		"spf":                spf,
		"sr":                 int64(audioSampleRate),
		"audioFormat":        audioFormat,
		"audioMode":          "default",
		"latencyMin":         int64(speakerLatencyMinSamples),
		"latencyMax":         int64(speakerLatencyMaxSamples),
	}
	if useAudioFEC(codec, chachaKey != nil) {
		streamBase["redundantAudio"] = int64(2)
	}
	buildSetup := func(layout audioConnectionLayout) map[string]interface{} {
		stream := clonePlistMap(streamBase)
		addScreenAudioStreamFields(stream, chachaKey, ctrlConn.LocalAddr().(*net.UDPAddr).Port, layout)
		// Music, not screen audio: the receiver plays it through its main
		// media path.
		stream["isMedia"] = true
		request := streamOnlyPlist(stream)
		if !sessionFirst {
			request = setupRequest.legacyStreamPlist(stream)
		}
		if policy.fairPlayOnStreams() {
			addFairPlayRootFields(request, c.FpEkey, c.fpIV, true)
		}
		debugDumpPlist("speaker audio setup plist", request)
		return request
	}
	layout := policy.audioConnections
	dbg("[SETUP] speaker phase 2 (audio): ct=%d spf=%d audioFormat=0x%x latency=%d", ct, spf, audioFormat, latencySamples)
	audioResp, _, _, err := c.requestSetup(uri, "audio stream", buildSetup(layout))
	if err != nil && setupShapeRejected(err) {
		alternate := audioLayoutControlPort
		if layout == audioLayoutControlPort {
			alternate = audioLayoutStreamConnections
		}
		dbg("[SETUP] receiver rejected %s audio descriptor; retrying with %s", audioLayoutName(layout), audioLayoutName(alternate))
		audioResp, _, _, err = c.requestSetup(uri, "audio stream alternate descriptor", buildSetup(alternate))
	}
	if err != nil {
		return nil, err
	}
	if err := observe(audioResp); err != nil {
		return nil, err
	}
	if eventConn == nil {
		dbg("[EVENT] receiver omitted or declined the optional event channel; continuing without it")
	}
	if !sessionFirst {
		if err := record(audioResp); err != nil {
			return nil, err
		}
	}

	dataPort, controlPort := 0, 0
	if streams, ok := audioResp["streams"].([]interface{}); ok {
		for _, s := range streams {
			if stream, ok := s.(map[string]interface{}); ok && plistInt(stream["type"]) == 96 {
				dataPort, controlPort = plistStreamPorts(stream)
			}
		}
	}
	if dataPort == 0 || controlPort == 0 {
		return nil, fmt.Errorf("audio SETUP response has no data and control ports")
	}

	// There is no video frame to wait for; StreamAudio starts right away.
	started := make(chan struct{})
	close(started)
	session := &MirrorSession{
		client:         c,
		eventConn:      eventConn,
		timingConn:     timingConn,
		cancel:         cancelSession,
		sessionURI:     uri,
		firstFrameSent: started,
		timingProtocol: timingProtocolNTP,
	}
	session.audioStream, err = session.setupAudioStream(dataPort, controlPort, aesKey, aesIV, chachaKey,
		policy.audioSecurity, byte(codec), latencySamples, ctrlConn, dataConn)
	if err != nil {
		return nil, err
	}
	session.startWorker(func() { session.feedbackLoop(sessionCtx) })

	setupSucceeded = true
	return session, nil
}
