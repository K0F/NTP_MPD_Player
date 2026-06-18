package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/beevik/ntp"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fhs/gompd/v2/mpd"
)

type statusMsg mpd.Attrs
type playlistMsg []mpd.Attrs
type errMsg error
type fzfResultMsg []string
type ntpOffsetMsg time.Duration // <--- Custom type for asynchronous NTP response

type model struct {
	client            *mpd.Client
	playlist          []mpd.Attrs
	currentStatus     mpd.Attrs
	err               error
	lastSongID        string
	cursor            int
	musicDir          string
	clockOffset       time.Duration // <--- Now tracks full precision duration
	ntpStatus         string        // Displays status of cosmic synchronization
	cursorInitialized bool
}

func initialModel() model {
	c, err := mpd.Dial("tcp", "localhost:6600")
	if err != nil {
		log.Fatal("Could not connect to MPD:", err)
	}

	musicPath := os.Getenv("HOME") + "/Music"

	return model{
		client:      c,
		cursor:      0,
		musicDir:    musicPath,
		clockOffset: 0,
		ntpStatus:   "Requesting atomic alignment...",
	}
}

// --- Asynchronous NTP Clock Ingestion ---
func fetchNTP() tea.Cmd {
	return func() tea.Msg {
		// Hit global pool with a strict 3-second network drop timeout
		response, err := ntp.QueryWithOptions("pool.ntp.org", ntp.QueryOptions{Timeout: 3 * time.Second})
		if err != nil {
			// Fallback to zero offset if network drops, preventing a crash
			return ntpOffsetMsg(0)
		}
		// This contains the exact difference between system clock and atomic truth
		return ntpOffsetMsg(response.ClockOffset)
	}
}
func syncEngine(client *mpd.Client, offset time.Duration) tea.Cmd {
	return func() tea.Msg {
		status, err := client.Status()
		if err != nil {
			return errMsg(err)
		}

		if status["state"] == "play" {
			trueTime := time.Now().Add(offset)
			targetSecondOfSystem := float64(trueTime.Second()) + float64(trueTime.Nanosecond())/1e9

			mpdElapsed, _ := strconv.ParseFloat(status["elapsed"], 64)
			songPos, _ := strconv.Atoi(status["song"])

			trackSecond := math.Mod(mpdElapsed, 60)

			drift := targetSecondOfSystem - trackSecond
			if drift < -30 {
				drift += 60
			} else if drift > 30 {
				drift -= 60
			}

			if drift > 0.5 || drift < -0.5 {
				idealTrackPosition := mpdElapsed + drift
				targetAbsolute := int(math.Round(idealTrackPosition))
				if targetAbsolute < 0 {
					targetAbsolute = 0
				}
				_ = client.Seek(songPos, targetAbsolute)
			}
		}

		// FIX: Increase this from 200ms to 500ms (or 1000ms for absolute minimum resource usage)
		// This stops hammering Termux's UI thread and makes key presses instant!
		time.Sleep(500 * time.Millisecond) 
		return statusMsg(status)
	}
}

func fetchPlaylist(client *mpd.Client) tea.Cmd {
	return func() tea.Msg {
		list, err := client.PlaylistInfo(-1, -1)
		if err != nil {
			return errMsg(err)
		}
		return playlistMsg(list)
	}
}
func runFzf(musicDir string) tea.Cmd {
	return tea.ExecProcess(exec.Command("sh", "-c", fmt.Sprintf(
		"cd %s && find . -type f -not -path '*/.*' | fzf -m > $HOME/observatory_fzf.txt",
		musicDir,
	)), func(err error) tea.Msg {
		if err != nil {
			return errMsg(err)
		}

		// Read from the new Termux-safe home directory path
		homeDir := os.Getenv("HOME")
		content, err := os.ReadFile(homeDir + "/observatory_fzf.txt")
		if err != nil || len(content) == 0 {
			return fzfResultMsg(nil)
		}

		lines := strings.Split(string(content), "\n")
		var tracks []string

		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if len(line) > 2 && line[:2] == "./" {
				line = line[2:]
			}
			tracks = append(tracks, line)
		}

		return fzfResultMsg(tracks)
	})
}

// Fire off both the audio engine AND the network time query on boot
func (m model) Init() tea.Cmd {
	return tea.Batch(
		fetchPlaylist(m.client),
		fetchNTP(),
		syncEngine(m.client, m.clockOffset),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case ntpOffsetMsg:
		m.clockOffset = time.Duration(msg)
		if m.clockOffset == 0 {
			m.ntpStatus = "Offline (Using raw hardware clock)"
		} else {
			m.ntpStatus = fmt.Sprintf("Synced! Latency Matrix Corrected: %.3fs", m.clockOffset.Seconds())
		}
		// Kick the sync engine immediately with the freshly calibrated offset
		return m, syncEngine(m.client, m.clockOffset)

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.client.Close()
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < len(m.playlist)-1 {
				m.cursor++
			}
			/*
				case "enter":
					if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
						trueTime := time.Now().Add(m.clockOffset)
						_ = m.client.Seek(m.cursor, trueTime.Second())
					}

			*/

		case "enter":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
				_ = m.client.Play(m.cursor)
			}

		case "d":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
				_ = m.client.Delete(m.cursor, -1)
				if m.cursor >= len(m.playlist)-1 && m.cursor > 0 {
					m.cursor--
				}
				return m, fetchPlaylist(m.client)
			}

		case "a":
			return m, runFzf(m.musicDir)

		// Manual overrides to micro-tune OS sound card buffer latency delays!
		case "+", "=":
			m.clockOffset += time.Second
			m.ntpStatus = fmt.Sprintf("Manual Tweak: %.3fs", m.clockOffset.Seconds())
		case "-":
			m.clockOffset -= time.Second
			m.ntpStatus = fmt.Sprintf("Manual Tweak: %.3fs", m.clockOffset.Seconds())
		}

	case playlistMsg:
		m.playlist = msg

	case fzfResultMsg:
		if len(msg) > 0 {
			for _, track := range msg {
				_ = m.client.Add(track)
			}
			_ = os.Remove("/tmp/observatory_fzf.txt")
			return m, fetchPlaylist(m.client)
		}

	case statusMsg:
		m.currentStatus = mpd.Attrs(msg)
		currentSongID := m.currentStatus["songid"]
		songPos, _ := strconv.Atoi(m.currentStatus["song"])

		// 1. STARTUP ALIGNMENT: Run this EXACTLY once on app boot, then lock it down.
		if !m.cursorInitialized {
			if currentSongID != "" {
				m.cursor = songPos
				m.lastSongID = currentSongID
				m.cursorInitialized = true
			}
		}

		// 2. TRACK CHANGEOVER: Only sync if we have a valid, non-empty song ID.
		// This completely blocks momentary seek-flickers from resetting your menu.
		if currentSongID != "" && currentSongID != m.lastSongID && m.playlist != nil {
			trueTime := time.Now().Add(m.clockOffset)
			_ = m.client.Seek(songPos, trueTime.Second())
			m.lastSongID = currentSongID
		}

		return m, syncEngine(m.client, m.clockOffset)

	case errMsg:
		m.err = msg
		return m, nil
	}

	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n Error: %v\n\nPress Q to quit.", m.err)
	}

	s := "╔══════════════════════════════════════════════════════════════╗\n"
	s += "║  OBSERVATORY COMPANION PLAYER (NTP EDITION)                  ║\n"
	s += "╚══════════════════════════════════════════════════════════════╝\n\n"

	// Compute virtual wall clock face
	trueTime := time.Now().Add(m.clockOffset)
	s += fmt.Sprintf(" [Master Clock Anchor]: %ds / 60s\n", trueTime.Second())
	s += fmt.Sprintf(" [NTP Quantum Status ]: %s\n\n", m.ntpStatus)
	s += " CURRENT PLAYLIST:\n ─────────────────\n"

	currentSongIndex, _ := strconv.Atoi(m.currentStatus["song"])

	for i, track := range m.playlist {
		title := track["title"]
		if title == "" {
			title = track["file"]
		}

		prefix := "   "
		if i == m.cursor {
			prefix = " > "
		}

		if i == currentSongIndex && m.currentStatus["state"] == "play" {
			if i == m.cursor {
				s += fmt.Sprintf("\033[1;32m%s[%2d] %s (playing)\033[0m\n", prefix, i, title)
			} else {
				s += fmt.Sprintf("   \033[1;32m[%2d] %s\033[0m\n", i, title)
			}
		} else {
			if i == m.cursor {
				s += fmt.Sprintf("\033[1;37m%s[%2d] %s\033[0m\n", prefix, i, title)
			} else {
				s += fmt.Sprintf("%s[%2d] %s\n", prefix, i, title)
			}
		}
	}

	if len(m.playlist) == 0 {
		s += "  (Playlist is completely empty. Press 'a' to grab files!)\n"
	}

	s += "\n ─────────────────\n"
	s += " [↑/↓] Move | [Enter] Play | [a] Add | [d] Delete | [+/-] Buffer Tweak | [q] Quit\n"

	return s
}

func main() {
	p := tea.NewProgram(initialModel())
	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
