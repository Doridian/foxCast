package applink

import "testing"

func TestResolve(t *testing.T) {
	const watch = "youtube://www.youtube.com/watch?v=dQw4w9WgXcQ"
	tests := []struct {
		in   string
		want Link
		ok   bool
	}{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", Link{"YouTube", watch}, true},
		{"https://youtube.com/watch?v=dQw4w9WgXcQ&t=42s&list=PL123", Link{"YouTube", watch}, true},
		{"http://m.youtube.com/watch?feature=share&v=dQw4w9WgXcQ", Link{"YouTube", watch}, true},
		{"https://youtu.be/dQw4w9WgXcQ?si=abc", Link{"YouTube", watch}, true},
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", Link{"YouTube", watch}, true},
		{"https://www.youtube.com/live/dQw4w9WgXcQ?feature=share", Link{"YouTube", watch}, true},
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", Link{"YouTube", watch}, true},
		{"https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ", Link{"YouTube", watch}, true},
		{" https://WWW.YouTube.com/watch?v=dQw4w9WgXcQ ", Link{"YouTube", watch}, true},
		{watch, Link{"YouTube", watch}, true},
		{"https://www.youtube.com/watch?v=short", Link{}, false},
		{"https://www.youtube.com/@channel", Link{}, false},
		{"https://www.youtube.com/playlist?list=PL123", Link{}, false},
		{"https://www.hulu.com/watch/73df1fcf-09ed-4717-a9d2-14d3138afd7e", Link{"Hulu", "hulu://watch/73df1fcf-09ed-4717-a9d2-14d3138afd7e"}, true},
		{"https://www.hulu.com/series/some-show-73DF1FCF-09ED-4717-A9D2-14D3138AFD7E", Link{"Hulu", "hulu://series/73df1fcf-09ed-4717-a9d2-14d3138afd7e"}, true},
		{"https://www.hulu.com/hub/movies", Link{}, false},
		{"https://tv.apple.com/us/movie/spirited/umc.cmc.3lp7wqowerzdbej98tveildi3", Link{"Apple TV", "https://tv.apple.com/us/movie/spirited/umc.cmc.3lp7wqowerzdbej98tveildi3"}, true},
		{"https://www.disneyplus.com/video/afdc98f1-26bf-48d8-8866-af185ba5d5ac", Link{"Disney+", "https://www.disneyplus.com/video/afdc98f1-26bf-48d8-8866-af185ba5d5ac"}, true},
		{"https://pluto.tv/us/live-tv/65d92a8c8b24c80008e285c0", Link{"Pluto TV", "https://pluto.tv/us/live-tv/65d92a8c8b24c80008e285c0"}, true},
		{"https://www.twitch.tv/somechannel", Link{}, false},
		{"https://example.com/video.mp4", Link{}, false},
		{"/home/user/video.mkv", Link{}, false},
		{"rtsp://youtube.com/watch?v=dQw4w9WgXcQ", Link{}, false},
	}
	for _, tt := range tests {
		got, ok := Resolve(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("Resolve(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
