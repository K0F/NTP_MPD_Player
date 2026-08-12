package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderHeader(t *testing.T) {
	t.Run("off air", func(t *testing.T) {
		got := renderHeader("0.2", false, 84)
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

	t.Run("on air", func(t *testing.T) {
		got := renderHeader("0.2", true, 84)
		if !strings.Contains(got, "\033[1;31m[ ON AIR ]\033[0m") {
			t.Errorf("on-air status not red/bold: %q", got)
		}
		if !strings.Contains(got, "[ ON AIR ]") {
			t.Errorf("header missing ON AIR status: %q", got)
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
		got := strip(renderHeader("0.2", false, 100))
		line := strings.TrimPrefix(got, "\n")
		line = strings.TrimSuffix(line, "\n")
		first, _, _ := strings.Cut(line, "\n")
		if len(first) != 100 {
			t.Errorf("header line length = %d, want %d: %q", len(first), 100, first)
		}
	})

	t.Run("minimum fill width", func(t *testing.T) {
		got := renderHeader("0.2", false, 10)
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
	header := renderHeader("1.0", true, 80)
	bar := renderProgressBar(30, 120, 20)
	combined := fmt.Sprintf("%s%s", header, bar)
	if combined == "" {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(header, "ON AIR") {
		t.Errorf("header should advertise ON AIR: %q", header)
	}
	if !strings.Contains(bar, "] 0:30 / 2:00") {
		t.Errorf("bar should show 0:30 / 2:00: %q", bar)
	}
}
