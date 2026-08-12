package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fhs/gompd/v2/mpd"
)

func TestParseMusicDir(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "quoted value with tab separation",
			config: "music_directory\t\t\"~/Music\"\n",
			want:   "~/Music",
		},
		{
			name:   "quoted value with spaces",
			config: "music_directory \t \"/mnt/data/My Music\"\n",
			want:   "/mnt/data/My Music",
		},
		{
			name:   "unquoted value",
			config: "music_directory /srv/audio\n",
			want:   "/srv/audio",
		},
		{
			name: "mixed config with comments",
			config: "# music_directory is here\n" +
				"bind_to_address \t\"/tmp/mpd.sock\"\n" +
				"music_directory\t\t\"/home/me/music\"\n" +
				"playlist_directory \"/var/lib/mpd/playlists\"\n",
			want: "/home/me/music",
		},
		{
			name:   "value with trailing comment ignored",
			config: "music_directory \"/srv/mp3\" # primary store\n",
			want:   "/srv/mp3",
		},
		{
			name:   "missing returns empty",
			config: "bind_to_address \"localhost\"\n",
			want:   "",
		},
		{
			name:   "empty config returns empty",
			config: "",
			want:   "",
		},
		{
			name:   "prefix only returns empty",
			config: "music_directory\n",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMusicDir(tt.config)
			if got != tt.want {
				t.Errorf("parseMusicDir(%q) = %q, want %q", tt.config, got, tt.want)
			}
		})
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("no home dir: %v", err)
	}

	tests := []struct {
		in   string
		want string
	}{
		{"~/Music", filepath.Join(home, "Music")},
		{"~", home},
		{"/mnt/data/recordings", "/mnt/data/recordings"},
		{"relative/path", "relative/path"},
	}
	for _, tt := range tests {
		if got := expandPath(tt.in); got != tt.want {
			t.Errorf("expandPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMPDMusicDirFallsBack(t *testing.T) {
	// With no config present in the standard locations, must not panic and
	// return empty so initialModel can apply its own fallback.
	oldXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Unsetenv("XDG_CONFIG_HOME")
	t.Cleanup(func() {
		if oldXDG != "" {
			os.Setenv("XDG_CONFIG_HOME", oldXDG)
		}
	})
	got := mpdMusicDir()
	_ = got // result depends on the machine; just ensure no panic
}

func TestFindAudioExpr(t *testing.T) {
	expr := findAudioExpr()
	for _, ext := range audioExts {
		if !strings.Contains(expr, "*."+ext) {
			t.Errorf("audio expr missing extension %q: %q", ext, expr)
		}
	}
	if !strings.Contains(expr, " -o ") {
		t.Errorf("audio expr should chain with -o: %q", expr)
	}
}

// TestAddTrackToPlaylistIntegration exercises the real add path against a live
// MPD daemon. It is skipped unless MPD_INTEGRATION=1 is set and the daemon is
// idle (not playing, empty playlist) so the queue is never disturbed.
func TestAddTrackToPlaylistIntegration(t *testing.T) {
	if os.Getenv("MPD_INTEGRATION") == "" {
		t.Skip("set MPD_INTEGRATION=1 to run live MPD integration test")
	}

	client, err := mpd.Dial("tcp", "localhost:6600")
	if err != nil {
		t.Skip("MPD not reachable on localhost:6600:", err)
	}
	defer client.Close()

	status, err := client.Status()
	if err != nil {
		t.Skip("MPD status failed:", err)
	}
	if state := status["state"]; state == "play" || state == "pause" {
		t.Skip("MPD is playing; not disturbing the queue")
	}
	playlist, err := client.PlaylistInfo(-1, -1)
	if err != nil || len(playlist) > 0 {
		t.Skip("playlist not empty; not disturbing it")
	}

	files, err := client.GetFiles()
	if err != nil || len(files) == 0 {
		t.Skip("no files in MPD database")
	}

	defer func() { _ = client.Clear() }()

	track := files[0]
	if err := addTrackToPlaylist(client, track); err != nil {
		t.Fatalf("addTrackToPlaylist(%q) failed: %v", track, err)
	}
	pl, err := client.PlaylistInfo(-1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl) != 1 {
		t.Fatalf("expected 1 track in playlist, got %d", len(pl))
	}
}

func TestRenderHeader(t *testing.T) {
	t.Run("off air", func(t *testing.T) {
		got := renderHeader("0.2", false, -1, 84)
		wantPrefix := "\n // NTP TERMINAL MPD PLAYER v0.2 "
		if !strings.HasPrefix(got, wantPrefix) {
			t.Errorf("header = %q, want prefix %q", got, wantPrefix)
		}
		if !strings.Contains(got, "[ OFF AIR ]") {
			t.Errorf("header missing OFF AIR status: %q", got)
		}
		if !strings.Contains(got, "\033[90m[ OFF AIR ]\033[0m") {
			t.Errorf("off-air status not dim-colored: %q", got)
		}
	})

	t.Run("on air shows listeners", func(t *testing.T) {
		got := renderHeader("0.2", true, 5, 84)
		if !strings.Contains(got, "\033[1;31m[ ON AIR: 5 ]\033[0m") {
			t.Errorf("on-air status missing listener count: %q", got)
		}
		if !strings.Contains(got, "[ ON AIR: 5 ]") {
			t.Errorf("header missing ON AIR: 5 status: %q", got)
		}
	})

	t.Run("on air unknown listeners", func(t *testing.T) {
		got := renderHeader("0.2", true, -1, 84)
		if !strings.Contains(got, "[ ON AIR: ? ]") {
			t.Errorf("header should show unknown listener count: %q", got)
		}
	})

	t.Run("fills to requested width", func(t *testing.T) {
		// Strip ANSI codes and count visible line width.
		strip := func(s string) string {
			for {
				start := strings.Index(s, "\033[")
				if start == -1 {
					return s
				}
				end := strings.Index(s[start:], "m")
				if end == -1 {
					return s
				}
				s = s[:start] + s[start+end+1:]
			}
		}
		got := strip(renderHeader("0.2", false, -1, 100))
		line := strings.TrimPrefix(got, "\n")
		line = strings.TrimSuffix(line, "\n")
		first, _, _ := strings.Cut(line, "\n")
		if len(first) != 100 {
			t.Errorf("header line length = %d, want %d: %q", len(first), 100, first)
		}
	})

	t.Run("minimum fill width", func(t *testing.T) {
		got := renderHeader("0.2", false, -1, 10)
		if !strings.HasPrefix(got, "\n // ") {
			t.Errorf("narrow header malformed: %q", got)
		}
	})
}

func TestRenderProgressBar(t *testing.T) {
	tests := []struct {
		name    string
		elapsed float64
		total   float64
		width   int
		wantBar string
		want    string
	}{
		{
			name:    "zero total returns empty",
			elapsed: 10,
			total:   0,
			width:   20,
			wantBar: "",
			want:    "",
		},
		{
			name:    "negative total returns empty",
			elapsed: 10,
			total:   -5,
			width:   20,
			wantBar: "",
			want:    "",
		},
		{
			name:    "zero progress",
			elapsed: 0,
			total:   100,
			width:   10,
			wantBar: "----------",
			want:    "[----------] 0:00 / 1:40",
		},
		{
			name:    "full progress",
			elapsed: 100,
			total:   100,
			width:   10,
			wantBar: "##########",
			want:    "[##########] 1:40 / 1:40",
		},
		{
			name:    "half progress rounds",
			elapsed: 50,
			total:   100,
			width:   10,
			wantBar: "#####-----",
			want:    "[#####-----] 0:50 / 1:40",
		},
		{
			name:    "clamps below zero",
			elapsed: -10,
			total:   100,
			width:   10,
			wantBar: "----------",
			want:    "[----------] 0:00 / 1:40",
		},
		{
			name:    "clamps above total",
			elapsed: 500,
			total:   100,
			width:   10,
			wantBar: "##########",
			want:    "[##########] 8:20 / 1:40",
		},
		{
			name:    "minute rollover",
			elapsed: 65,
			total:   130,
			width:   20,
			wantBar: "##########----------",
			want:    "[##########----------] 1:05 / 2:10",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderProgressBar(tt.elapsed, tt.total, tt.width)
			if got != tt.want {
				t.Errorf("renderProgressBar(%v, %v, %d) = %q, want %q", tt.elapsed, tt.total, tt.width, got, tt.want)
			}
		})
	}

	t.Run("bar matches width", func(t *testing.T) {
		got := renderProgressBar(0.5, 100, 25)
		barStart := strings.Index(got, "[")
		barEnd := strings.Index(got, "]")
		if barStart == -1 || barEnd == -1 {
			t.Fatalf("malformed bar: %q", got)
		}
		if barEnd-barStart-1 != 25 {
			t.Errorf("bar width = %d, want 25", barEnd-barStart-1)
		}
	})
}

func TestRenderHeaderAndBarFormat(t *testing.T) {
	header := renderHeader("1.0", true, 3, 80)
	bar := renderProgressBar(30, 120, 20)
	combined := fmt.Sprintf("%s%s", header, bar)
	if combined == "" {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(header, "ON AIR: 3") {
		t.Errorf("header should advertise ON AIR with listeners: %q", header)
	}
	if !strings.Contains(bar, "] 0:30 / 2:00") {
		t.Errorf("bar should show 0:30 / 2:00: %q", bar)
	}
}

func TestParseIcecastListeners(t *testing.T) {
	const mount = "/radio.mp3"
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "single source object",
			body: `{"icestats":{"source":{"listeners":3,"listenurl":"http://x:9000/radio.mp3"}}}`,
			want: 3,
		},
		{
			name: "source array picks matching mount",
			body: `{"icestats":{"source":[
				{"listeners":1,"listenurl":"http://x:9000/other"},
				{"listeners":7,"listenurl":"http://x:9000/radio.mp3"}]}}`,
			want: 7,
		},
		{
			name: "source array falls back to first",
			body: `{"icestats":{"source":[
				{"listeners":2,"listenurl":"http://x:9000/one"},
				{"listeners":4,"listenurl":"http://x:9000/two"}]}}`,
			want: 2,
		},
		{
			name: "no source",
			body: `{"icestats":{"host":"x"}}`,
			want: 0,
		},
		{
			name: "empty source array",
			body: `{"icestats":{"source":[]}}`,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIcecastListeners([]byte(tt.body), mount)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("parseIcecastListeners() = %d, want %d", got, tt.want)
			}
		})
	}

	t.Run("malformed json errors", func(t *testing.T) {
		if _, err := parseIcecastListeners([]byte("{nope"), mount); err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 20, "short"},
		{"hello", 5, "hello"},
		{"hello", 3, "he~"},
		{"hello", 1, "h"},
		{"hello", 0, ""},
		{"héllo", 4, "hél~"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		if got := truncate(tt.in, tt.max); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestFormatDur(t *testing.T) {
	tests := []struct {
		sec  float64
		want string
	}{
		{0, "0:00"},
		{65, "1:05"},
		{59.9, "0:59"},
		{3600, "1:00:00"},
		{3725, "1:02:05"},
		{-10, "0:00"},
	}
	for _, tt := range tests {
		if got := formatDur(tt.sec); got != tt.want {
			t.Errorf("formatDur(%v) = %q, want %q", tt.sec, got, tt.want)
		}
	}
}

func TestFormatOffset(t *testing.T) {
	if got := formatOffset(12 * 1000 * 1000); got != "+12ms" {
		t.Errorf("formatOffset(+12ms) = %q", got)
	}
	if got := formatOffset(-5 * 1000 * 1000); got != "-5ms" {
		t.Errorf("formatOffset(-5ms) = %q", got)
	}
	if got := formatOffset(0); got != "+0ms" {
		t.Errorf("formatOffset(0) = %q", got)
	}
}

func TestTrackTitle(t *testing.T) {
	if got := trackTitle(mpd.Attrs{"artist": "A", "title": "T"}); got != "A - T" {
		t.Errorf("trackTitle() = %q", got)
	}
	if got := trackTitle(mpd.Attrs{"title": "Only"}); got != "Only" {
		t.Errorf("trackTitle() = %q", got)
	}
	if got := trackTitle(mpd.Attrs{"file": "dir/file.mp3"}); got != "file.mp3" {
		t.Errorf("trackTitle() = %q", got)
	}
}

func TestRenderTrackRow(t *testing.T) {
	strip := func(s string) string {
		for {
			start := strings.Index(s, "\033[")
			if start == -1 {
				return s
			}
			end := strings.Index(s[start:], "m")
			if end == -1 {
				return s
			}
			s = s[:start] + s[start+end+1:]
		}
	}

	t.Run("fits width exactly", func(t *testing.T) {
		track := mpd.Attrs{"title": "Some Long Title", "duration": "312"}
		got := strip(renderTrackRow(track, 4, 100, 3, false, 60))
		if len(got) != 60 {
			t.Errorf("row length = %d, want 60: %q", len(got), got)
		}
		if !strings.Contains(got, "[5:12]") {
			t.Errorf("row missing duration: %q", got)
		}
	})

	t.Run("cursor marker", func(t *testing.T) {
		track := mpd.Attrs{"title": "T"}
		got := renderTrackRow(track, 1, 1, 0, false, 40)
		if !strings.HasPrefix(got, "\033[7m > 1. ") {
			t.Errorf("cursor row should be reversed and marked: %q", got)
		}
	})

	t.Run("playing marker", func(t *testing.T) {
		track := mpd.Attrs{"title": "T"}
		got := renderTrackRow(track, 2, 2, 0, true, 40)
		if !strings.HasPrefix(got, "\033[32m   2. * ") {
			t.Errorf("playing row should be green with star: %q", got)
		}
	})

	t.Run("no duration", func(t *testing.T) {
		track := mpd.Attrs{"title": "T"}
		got := strip(renderTrackRow(track, 1, 1, 0, false, 30))
		if strings.Contains(got, "[") {
			t.Errorf("row should have no duration: %q", got)
		}
	})

	t.Run("multi-byte title does not panic and fits width", func(t *testing.T) {
		track := mpd.Attrs{"title": "Inner⧸Outside-JihB-YbZ_ec — Привет", "duration": "312"}
		got := strip(renderTrackRow(track, 5, 100, 4, false, 60))
		if len([]rune(got)) != 60 {
			t.Errorf("row rune length = %d, want 60: %q", len([]rune(got)), got)
		}
	})

	t.Run("narrow width", func(t *testing.T) {
		track := mpd.Attrs{"title": "A Very Long Track Title Here", "duration": "312"}
		for _, w := range []int{10, 15} {
			_ = strip(renderTrackRow(track, 5, 100, 4, false, w)) // must not panic
		}
		for _, w := range []int{20, 30} {
			got := strip(renderTrackRow(track, 5, 100, 4, false, w))
			if len([]rune(got)) != w {
				t.Errorf("row rune length = %d, want %d: %q", len([]rune(got)), w, got)
			}
		}
	})
}
