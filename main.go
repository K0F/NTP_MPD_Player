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

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fhs/gompd/v2/mpd"
)

type statusMsg mpd.Attrs
type playlistMsg []mpd.Attrs
type errMsg error
type fzfResultMsg []string 

type model struct {
	client        *mpd.Client
	playlist      []mpd.Attrs
	currentStatus mpd.Attrs
	err           error
	lastSongID    string
	cursor        int
	musicDir      string 
}

func initialModel() model {
	c, err := mpd.Dial("tcp", "localhost:6600")
	if err != nil {
		log.Fatal("Could not connect to MPD:", err)
	}

	musicPath := os.Getenv("HOME") + "/Music"

	return model{
		client:   c,
		cursor:   0,
		musicDir: musicPath,
	}
}

// --- Background Core Loop Engine (Fixed Minute-Boundary Math) ---
func syncEngine(client *mpd.Client) tea.Cmd {
	return func() tea.Msg {
		status, err := client.Status()
		if err != nil {
			return errMsg(err)
		}

		if status["state"] == "play" {
			now := time.Now()
			targetSecondOfSystem := now.Second()

			mpdElapsed, _ := strconv.ParseFloat(status["elapsed"], 64)
			songPos, _ := strconv.Atoi(status["song"])

			// 1. Isolate just the sub-minute seconds of the track (0.0 to 59.99)
			trackSecond := math.Mod(mpdElapsed, 60)

			// 2. Calculate circular drift relative to the 60s clock face
			drift := float64(targetSecondOfSystem) - trackSecond
			if drift < -30 {
				drift += 60
			} else if drift > 30 {
				drift -= 60
			}

			// 3. Tight sync threshold
			if drift > 1.2 || drift < -1.2 {
				// Apply the drift correction directly to the absolute elapsed position 
				// to preserve what minute of the song we are currently playing.
				targetAbsolute := int(math.Round(mpdElapsed + drift))
				if targetAbsolute < 0 {
					targetAbsolute = 0
				}
				_ = client.Seek(songPos, targetAbsolute)
			}
		}

		time.Sleep(200 * time.Millisecond)
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
		"cd %s && find . -type f -not -path '*/.*' | fzf -m > /tmp/observatory_fzf.txt", 
		musicDir,
	)), func(err error) tea.Msg {
		if err != nil {
			return errMsg(err)
		}

		content, err := os.ReadFile("/tmp/observatory_fzf.txt")
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

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchPlaylist(m.client), syncEngine(m.client))
}

// --- The Update Loop ---
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.client.Close()
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			} else if len(m.playlist) > 0 {
				m.cursor = len(m.playlist) - 1
			}

		case "down", "j":
			if m.cursor < len(m.playlist)-1 {
				m.cursor++
			} else {
				m.cursor = 0
			}

		case "enter":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
				targetSecond := time.Now().Second()
				_ = m.client.Seek(m.cursor, targetSecond)
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

		if m.lastSongID == "" {
			m.lastSongID = currentSongID
			m.cursor = songPos
			return m, syncEngine(m.client)
		}

		if currentSongID != m.lastSongID && m.playlist != nil {
			targetSecond := time.Now().Second()
			_ = m.client.Seek(songPos, targetSecond)
			m.lastSongID = currentSongID
			m.cursor = songPos
		}

		return m, syncEngine(m.client)

	case errMsg:
		m.err = msg
		return m, nil
	}

	return m, nil
}

// --- The View (UI Layout) ---
func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n Error: %v\n\nPress Q to quit.", m.err)
	}

	s := "╔══════════════════════════════════════════════════════════════╗\n"
	s += "║  KOF's SYNC MPD PLAYER                                       ║\n"
	s += "╚══════════════════════════════════════════════════════════════╝\n\n"

	currentSec := time.Now().Second()
	s += fmt.Sprintf(" [Master Clock Anchor]: %ds / 60s\n\n", currentSec)
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
	s += " [↑/↓] Move | [Enter] Play | [a] Add Track (fzf) | [d] Delete | [q] Quit\n"

	return s
}

func main() {
	p := tea.NewProgram(initialModel())
	if _, err := p.Run(); err != nil {
		log.Fatal(err)
	}
}
