// Package ytsearch finds a YouTube video from what an Apple TV reports as
// playing (title, channel, duration): the Apple TV's now-playing metadata has
// no video ID, so the ID is looked up on YouTube's search page. The page's
// embedded ytInitialData is not a documented interface and may change; see
// docs/12-nowplaying.md.
package ytsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	searchURL = "https://www.youtube.com/results"
	// durationTolerance is how far a result's length may differ from the
	// reported duration (seconds; YouTube rounds the length it displays).
	durationTolerance = 2.0
	maxPageBytes      = 8 << 20
	// userAgent: the search page serves its data to ordinary browsers.
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"
)

var initialData = regexp.MustCompile(`(?s)var ytInitialData = (\{.*?\});</script>`)

// ErrNotFound means no search result matched the title and duration.
var ErrNotFound = errors.New("no matching YouTube video found")

// Video is a search result.
type Video struct {
	ID       string
	Title    string
	Channel  string
	Duration float64 // seconds; 0 for live streams
}

// Query describes the video to find. Channel and Duration are optional but
// narrow the match; a Duration of 0 is ignored.
type Query struct {
	Title    string
	Channel  string
	Duration float64
}

// Find searches YouTube for q and returns the best matching video.
func Find(ctx context.Context, client *http.Client, q Query) (Video, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(q.Title) == "" {
		return Video{}, errors.New("no title to search for")
	}
	terms := strings.TrimSpace(q.Title + " " + q.Channel)
	u := searchURL + "?hl=en&search_query=" + url.QueryEscape(terms)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Video{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Skips the EU consent interstitial, which has no results.
	req.Header.Set("Cookie", "CONSENT=YES+1")
	resp, err := client.Do(req)
	if err != nil {
		return Video{}, fmt.Errorf("YouTube search: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Video{}, fmt.Errorf("YouTube search: HTTP %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return Video{}, fmt.Errorf("YouTube search: %w", err)
	}
	videos, err := ParseResults(page)
	if err != nil {
		return Video{}, err
	}
	return Match(videos, q)
}

// ParseResults extracts the videos from a search results page.
func ParseResults(page []byte) ([]Video, error) {
	m := initialData.FindSubmatch(page)
	if m == nil {
		return nil, errors.New("YouTube search page has no result data (its format may have changed)")
	}
	var data any
	if err := json.Unmarshal(m[1], &data); err != nil {
		return nil, fmt.Errorf("YouTube search data: %w", err)
	}
	var videos []Video
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if r, ok := v["videoRenderer"].(map[string]any); ok {
				if video, ok := parseRenderer(r); ok {
					videos = append(videos, video)
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(data)
	return videos, nil
}

func parseRenderer(r map[string]any) (Video, bool) {
	id, _ := r["videoId"].(string)
	if id == "" {
		return Video{}, false
	}
	v := Video{ID: id, Title: runsText(r["title"]), Channel: runsText(r["ownerText"])}
	if length, ok := r["lengthText"].(map[string]any); ok {
		v.Duration = parseClock(fmt.Sprint(length["simpleText"]))
	}
	return v, true
}

// runsText joins a {"runs": [{"text": ...}]} text object.
func runsText(v any) string {
	obj, _ := v.(map[string]any)
	runs, _ := obj["runs"].([]any)
	var sb strings.Builder
	for _, run := range runs {
		if m, ok := run.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// parseClock converts "1:02:03" or "24:45" to seconds; 0 if malformed.
func parseClock(s string) float64 {
	total := 0
	for _, part := range strings.Split(s, ":") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return float64(total)
}

// Match picks the video that best fits q: the title must be equal ignoring
// case and surrounding space, the duration (if given) within a couple of
// seconds, and a matching channel wins over a different one.
func Match(videos []Video, q Query) (Video, error) {
	var best *Video
	bestScore := -1
	for i, v := range videos {
		if !strings.EqualFold(strings.TrimSpace(v.Title), strings.TrimSpace(q.Title)) {
			continue
		}
		if q.Duration > 0 && (v.Duration == 0 || math.Abs(v.Duration-q.Duration) > durationTolerance) {
			continue
		}
		score := 1
		if q.Channel != "" && strings.EqualFold(strings.TrimSpace(v.Channel), strings.TrimSpace(q.Channel)) {
			score = 2
		}
		if score > bestScore {
			best, bestScore = &videos[i], score
		}
	}
	if best == nil {
		return Video{}, ErrNotFound
	}
	return *best, nil
}

// WatchURL is the link that opens v at position seconds.
func WatchURL(id string, position float64) string {
	u := "https://www.youtube.com/watch?v=" + url.QueryEscape(id)
	if secs := int(position); secs > 0 {
		u += "&t=" + strconv.Itoa(secs) + "s"
	}
	return u
}
