package transmux

import (
	"fmt"
	"strings"
	"time"

	"git.foxden.network/FoxDen/foxCast/internal/mkv"
)

// Text subtitle tracks become WebVTT subtitle renditions. Bitmap formats
// (PGS, VobSub, DVB) cannot be carried: HLS on Apple devices accepts only
// WebVTT and IMSC1 text, so they would need OCR or burning into the video.

// Matroska subtitle codec IDs.
const (
	codecSRT       = "S_TEXT/UTF8"
	codecSRTASCII  = "S_TEXT/ASCII"
	codecASS       = "S_TEXT/ASS"
	codecSSA       = "S_TEXT/SSA"
	codecASSLegacy = "S_ASS"
	codecSSALegacy = "S_SSA"
	codecWebVTT    = "S_TEXT/WEBVTT"
	codecPGS       = "S_HDMV/PGS"
	codecVobSub    = "S_VOBSUB"
	codecDVBSub    = "S_DVBSUB"

	subtitleGroupID     = "subs"
	contentTypeWebVTT   = "text/vtt"
	subtitleSuffix      = ".vtt"
	defaultCueDuration  = 4 * time.Second
	assDialogueFields   = 9 // ReadOrder,Layer,Style,Name,MarginL,MarginR,MarginV,Effect,Text
	webVTTHeaderTimeMap = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n"
)

type subtitleFormat int

const (
	subtitleSRT subtitleFormat = iota
	subtitleASS
	subtitleWebVTT
)

// subtitleTrack is the mapping of a Matroska text subtitle track.
type subtitleTrack struct {
	src    *mkv.Track
	format subtitleFormat
	name   string
}

// newSubtitleTrack maps t, or explains why it cannot be offered.
func newSubtitleTrack(t *mkv.Track) (*subtitleTrack, error) {
	if err := t.Supported(); err != nil {
		return nil, err
	}
	st := &subtitleTrack{src: t}
	switch t.CodecID {
	case codecSRT, codecSRTASCII:
		st.format = subtitleSRT
	case codecASS, codecSSA, codecASSLegacy, codecSSALegacy:
		st.format = subtitleASS
	case codecWebVTT:
		st.format = subtitleWebVTT
	case codecPGS, codecVobSub, codecDVBSub:
		return nil, fmt.Errorf("%s subtitles are images; Apple TV only takes text subtitles (would need OCR)", subtitleDisplayName(t.CodecID))
	default:
		return nil, fmt.Errorf("unsupported subtitle codec %s", t.CodecID)
	}
	return st, nil
}

func subtitleDisplayName(codecID string) string {
	switch codecID {
	case codecPGS:
		return "PGS"
	case codecVobSub:
		return "VobSub"
	case codecDVBSub:
		return "DVB"
	case codecSRT, codecSRTASCII:
		return "SRT"
	case codecASS, codecSSA, codecASSLegacy, codecSSALegacy:
		return "ASS"
	case codecWebVTT:
		return "WebVTT"
	}
	return codecID
}

// webVTT renders one segment's cues. Timestamps are absolute on the media
// timeline, which starts at 0 like the fMP4 decode times.
func (st *subtitleTrack) webVTT(frames []*mkv.Frame) []byte {
	var b strings.Builder
	b.WriteString(webVTTHeaderTimeMap)
	for _, fr := range frames {
		text := st.cueText(fr.Data)
		if text == "" {
			continue
		}
		dur := fr.Duration
		if dur <= 0 {
			dur = defaultCueDuration
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n", vttTimestamp(fr.PTS), vttTimestamp(fr.PTS+dur), text)
	}
	return []byte(b.String())
}

// cueText converts one block's payload to WebVTT cue text.
func (st *subtitleTrack) cueText(data []byte) string {
	var text string
	switch st.format {
	case subtitleSRT:
		text = srtToVTT(string(data))
	case subtitleASS:
		text = assToVTT(string(data))
	case subtitleWebVTT:
		text = string(data)
	}
	// A blank line would end the cue early.
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimRight(l, " \t\r"))
		}
	}
	return strings.Join(lines, "\n")
}

func vttTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// vttEscape escapes text so it is not parsed as WebVTT markup.
func vttEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// srtToVTT keeps the <i>, <b> and <u> tags WebVTT shares with SRT, drops
// other tags (e.g. <font>) and escapes everything else.
func srtToVTT(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		lt := strings.IndexByte(s, '<')
		if lt < 0 {
			b.WriteString(vttEscape(s))
			break
		}
		b.WriteString(vttEscape(s[:lt]))
		gt := strings.IndexByte(s[lt:], '>')
		if gt < 0 {
			b.WriteString(vttEscape(s[lt:]))
			break
		}
		tag := strings.ToLower(s[lt+1 : lt+gt])
		if tag == "" || !(tag[0] == '/' || (tag[0] >= 'a' && tag[0] <= 'z')) {
			// Not markup (e.g. "a < b"): keep the '<' as text.
			b.WriteString("&lt;")
			s = s[lt+1:]
			continue
		}
		switch tag {
		case "i", "b", "u", "/i", "/b", "/u":
			b.WriteString("<" + tag + ">")
		}
		s = s[lt+gt+1:]
	}
	return b.String()
}

// assToVTT converts a Matroska ASS/SSA block (the Dialogue fields after
// Start/End) to cue text: override blocks are dropped except italic and
// bold, drawings are skipped, and \N/\n become line breaks.
func assToVTT(s string) string {
	fields := strings.SplitN(s, ",", assDialogueFields)
	if len(fields) < assDialogueFields {
		return ""
	}
	text := fields[assDialogueFields-1]
	var b strings.Builder
	italic, bold, drawing := false, false, false
	setStyle := func(tag string, on bool, state *bool) {
		if on != *state {
			if on {
				b.WriteString("<" + tag + ">")
			} else {
				b.WriteString("</" + tag + ">")
			}
			*state = on
		}
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				i = len(text)
				continue
			}
			for _, o := range strings.Split(text[i+1:i+end], `\`) {
				switch {
				case o == "i1":
					setStyle("i", true, &italic)
				case o == "i0" || o == "i":
					setStyle("i", false, &italic)
				case o == "b1":
					setStyle("b", true, &bold)
				case o == "b0" || o == "b":
					setStyle("b", false, &bold)
				case strings.HasPrefix(o, "p") && len(o) > 1 && o[1] >= '0' && o[1] <= '9':
					drawing = o != "p0"
				}
			}
			i += end
		case drawing:
		case c == '\\' && i+1 < len(text) && (text[i+1] == 'N' || text[i+1] == 'n'):
			b.WriteByte('\n')
			i++
		case c == '\\' && i+1 < len(text) && text[i+1] == 'h':
			b.WriteByte(' ')
			i++
		case c == '&' || c == '<' || c == '>':
			b.WriteString(vttEscape(string(rune(c))))
		default:
			b.WriteByte(c) // UTF-8 continuation bytes pass through intact
		}
	}
	setStyle("i", false, &italic)
	setStyle("b", false, &bold)
	return b.String()
}
