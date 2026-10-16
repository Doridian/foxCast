package ytsearch

import "testing"

const page = `<html><script>var ytInitialData = {"contents":{"items":[
{"videoRenderer":{"videoId":"AAAAAAAAAAA","title":{"runs":[{"text":"The Speedrunners Who Broke Mario Odyssey in Half"}]},"ownerText":{"runs":[{"text":"Someone Else"}]},"lengthText":{"simpleText":"24:45"}}},
{"videoRenderer":{"videoId":"EHkrgXQXcs8","title":{"runs":[{"text":"The Speedrunners Who Broke Mario Odyssey in Half"}]},"ownerText":{"runs":[{"text":"Ghost Pixel"}]},"lengthText":{"simpleText":"24:45"}}},
{"videoRenderer":{"videoId":"L5aQzCJW5-s","title":{"runs":[{"text":"Other"}]},"ownerText":{"runs":[{"text":"Ghost Pixel"}]},"lengthText":{"simpleText":"1:02:03"}}}
]}};</script></html>`

func TestFind(t *testing.T) {
	videos, err := ParseResults([]byte(page))
	if err != nil || len(videos) != 3 {
		t.Fatalf("ParseResults = %v, %v", videos, err)
	}
	if videos[2].Duration != 3723 {
		t.Errorf("duration = %v, want 3723", videos[2].Duration)
	}
	q := Query{Title: "the speedrunners who broke mario odyssey in half", Channel: "Ghost Pixel", Duration: 1485.0009}
	v, err := Match(videos, q)
	if err != nil || v.ID != "EHkrgXQXcs8" {
		t.Fatalf("Match = %v, %v; want the Ghost Pixel video", v, err)
	}
	q.Duration = 1400
	if _, err := Match(videos, q); err != ErrNotFound {
		t.Errorf("Match with wrong duration: err = %v, want ErrNotFound", err)
	}
	if _, err := ParseResults([]byte("<html></html>")); err == nil {
		t.Error("ParseResults accepted a page without data")
	}
}

func TestWatchURL(t *testing.T) {
	if got := WatchURL("abc", 83.9); got != "https://www.youtube.com/watch?v=abc&t=83s" {
		t.Errorf("WatchURL = %s", got)
	}
	if got := WatchURL("abc", 0); got != "https://www.youtube.com/watch?v=abc" {
		t.Errorf("WatchURL = %s", got)
	}
}
