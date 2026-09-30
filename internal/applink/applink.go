// Package applink maps web URLs of video sites to the links that open them
// in the site's Apple TV app. Only mappings with a documented source are
// listed; see docs/10-companion.md.
package applink

import (
	"net/url"
	"regexp"
	"strings"
)

// Link is a link an Apple TV app opens.
type Link struct {
	// App names the app, for messages.
	App string
	// URL is what to hand the Apple TV: a deep link or a universal link.
	URL string
}

// youTubeVideoID matches YouTube's 11-character video IDs.
var youTubeVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// hulu IDs are UUIDs.
var huluID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Resolve returns the app link for location, if it is a URL of a site with a
// known Apple TV app.
func Resolve(location string) (Link, bool) {
	u, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return Link{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch scheme {
	case "youtube":
		// Already a deep link.
		return Link{App: "YouTube", URL: u.String()}, true
	case "hulu":
		return Link{App: "Hulu", URL: u.String()}, true
	case "http", "https":
	default:
		return Link{}, false
	}

	switch host {
	case "youtube.com", "m.youtube.com", "youtu.be", "youtube-nocookie.com":
		if id, ok := youTubeVideo(host, u); ok {
			// The deep-link form reported to work on tvOS; see
			// docs/10-companion.md.
			return Link{App: "YouTube", URL: "youtube://www.youtube.com/watch?v=" + id}, true
		}
	case "hulu.com":
		// hulu.com/watch/<id> and hulu.com/series/<slug-id> -> hulu://watch/<id>
		// and hulu://series/<id>.
		kind, rest, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
		if kind == "watch" || kind == "series" {
			if id := trailingUUID(rest); id != "" {
				return Link{App: "Hulu", URL: "hulu://" + kind + "/" + id}, true
			}
		}
	case "tv.apple.com":
		// Universal links, handed over unchanged.
		return Link{App: "Apple TV", URL: u.String()}, true
	case "disneyplus.com":
		return Link{App: "Disney+", URL: u.String()}, true
	case "pluto.tv":
		return Link{App: "Pluto TV", URL: u.String()}, true
	}
	return Link{}, false
}

// youTubeVideo extracts the video ID from the URL forms YouTube shares:
// /watch?v=, /shorts/, /live/, /embed/, /v/ and youtu.be/.
func youTubeVideo(host string, u *url.URL) (string, bool) {
	path := strings.Trim(u.Path, "/")
	var id string
	switch {
	case host == "youtu.be":
		id, _, _ = strings.Cut(path, "/")
	case path == "watch":
		id = u.Query().Get("v")
	default:
		kind, rest, _ := strings.Cut(path, "/")
		switch kind {
		case "shorts", "live", "embed", "v":
			id, _, _ = strings.Cut(rest, "/")
		}
	}
	if !youTubeVideoID.MatchString(id) {
		return "", false
	}
	return id, true
}

// trailingUUID returns the UUID a Hulu path segment ends with
// ("some-show-<uuid>" or "<uuid>").
func trailingUUID(segment string) string {
	segment, _, _ = strings.Cut(segment, "/")
	if len(segment) < 36 {
		return ""
	}
	if id := segment[len(segment)-36:]; huluID.MatchString(id) {
		return strings.ToLower(id)
	}
	return ""
}
