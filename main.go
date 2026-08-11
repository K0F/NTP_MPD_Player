package main

import (
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fhs/gompd/v2/mpd"
)

var version string = "0.2"

// --- Bubble Tea Messages ---
type (
	statusMsg    mpd.Attrs
	playlistMsg  []mpd.Attrs
	fzfResultMsg []string
	errMsg       error
)

// --- Core Application Model ---
type model struct {
	client            *mpd.Client
	playlist          []mpd.Attrs
	currentStatus     mpd.Attrs
	err               error
	lastSongID        string
	cursor            int
	musicDir          string
	clockOffset       time.Duration
	ntpStatus         string
	cursorInitialized bool
	syncCooldownUntil time.Time // CRITICAL: State memory to block seek-storms
	termHeight        int       // Dynamická výška okna pro multipage výpočet
}

func preciseSeekRaw(targetSec float64) {
	conn, err := net.Dial("tcp", "localhost:6600")
	if err != nil {
		return // Fail silently to keep the user interface responsive
	}
	defer conn.Close()

	// Clear MPD's initial connection welcome handshake from the read buffer
	buf := make([]byte, 1024)
	if _, err := conn.Read(buf); err != nil {
		return
	}

	// seekcur modifies the playback timeline of the current track with float precision
	cmd := fmt.Sprintf("seekcur %.3f\n", targetSec)
	_, _ = conn.Write([]byte(cmd))
}

// --- Ensure Audio Outputs are Enabled ---
func ensureOutputsEnabled(client *mpd.Client) {
	outputs, err := client.ListOutputs()
	if err != nil {
		return
	}
	for _, out := range outputs {
		if enabled, ok := out["outputenabled"]; ok && (enabled == "0" || enabled == "false") {
			if idStr, ok := out["outputid"]; ok {
				if id, err := strconv.Atoi(idStr); err == nil {
					_ = client.EnableOutput(id)
				}
			}
		}
	}
}

// --- Toggle Broadcast (Icecast / Second MPD Output) ---
func toggleBroadcast(client *mpd.Client) (bool, error) {
	outputs, err := client.ListOutputs()
	if err != nil {
		return false, err
	}
	var targetID int = -1
	var currentlyEnabled bool = false

	for _, out := range outputs {
		idStr, hasID := out["outputid"]
		name, _ := out["outputname"]
		enabled, _ := out["outputenabled"]

		if hasID && (strings.Contains(strings.ToLower(name), "icecast") || strings.Contains(strings.ToLower(name), "stream") || idStr == "1") {
			if id, err := strconv.Atoi(idStr); err == nil {
				targetID = id
				currentlyEnabled = (enabled == "1" || enabled == "true")
				break
			}
		}
	}

	if targetID == -1 && len(outputs) > 1 {
		if idStr, ok := outputs[1]["outputid"]; ok {
			if id, err := strconv.Atoi(idStr); err == nil {
				targetID = id
				enabled, _ := outputs[1]["outputenabled"]
				currentlyEnabled = (enabled == "1" || enabled == "true")
			}
		}
	}

	if targetID == -1 {
		return false, fmt.Errorf("no radio stream output found")
	}

	if currentlyEnabled {
		err := client.DisableOutput(targetID)
		return false, err
	} else {
		err := client.EnableOutput(targetID)
		return true, err
	}
}

// --- Check if Broadcast output is active ---
func isBroadcastActive(client *mpd.Client) bool {
	outputs, err := client.ListOutputs()
	if err != nil {
		return false
	}
	for _, out := range outputs {
		idStr, _ := out["outputid"]
		name, _ := out["outputname"]
		enabled, _ := out["outputenabled"]

		if strings.Contains(strings.ToLower(name), "icecast") || strings.Contains(strings.ToLower(name), "stream") || idStr == "1" {
			return enabled == "1" || enabled == "true"
		}
	}
	return false
}

// --- Dumb Background Poller ---
func syncEngine(client *mpd.Client) tea.Cmd {
	return func() tea.Msg {
		// Throttle polling thread to keep Termux light and responsive
		time.Sleep(500 * time.Millisecond)
		status, err := client.Status()
		if err != nil {
			return errMsg(err)
		}
		return statusMsg(status)
	}
}

// --- Fetch Current MPD Playlist ---
func fetchPlaylist(client *mpd.Client) tea.Cmd {
	return func() tea.Msg {
		list, err := client.PlaylistInfo(-1, -1)
		if err != nil {
			return errMsg(err)
		}
		return playlistMsg(list)
	}
}

// --- Termux Native FZF File Browser ---
func runFzf(musicDir string) tea.Cmd {
	return tea.ExecProcess(exec.Command("sh", "-c", fmt.Sprintf(
		"cd %s && find . -type f -not -path '*/.*' | fzf -m > $HOME/observatory_fzf.txt",
		musicDir,
	)), func(err error) tea.Msg {
		// If user presses Esc in fzf, it exits with status 1. Ignore this as a non-fatal cancellation.
		if err != nil {
			// Check if it's just a normal fzf exit (cancelled)
			homeDir := os.Getenv("HOME")
			_ = os.Remove(homeDir + "/observatory_fzf.txt")
			return fzfResultMsg(nil)
		}

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

// --- Application Initialization ---
func initialModel(ntpOffset time.Duration) model {
	c, err := mpd.Dial("tcp", "localhost:6600")
	if err != nil {
		log.Fatal("Could not connect to MPD local daemon:", err)
	}

	//musicPath := os.Getenv("HOME") + "/Music"
	musicPath := "/mnt/data/recordings"

	var hardwareLatency time.Duration = 0 * time.Millisecond
	ntpStatusMsg := "NTP Sync: Active"

	// Native Termux/Android Hardware Calibration
	if os.Getenv("TERMUX_VERSION") != "" {
		hardwareLatency = 450 * time.Millisecond
		ntpStatusMsg = "NTP + Android Hardware Audio Profile Active (+0.450s)"
		musicPath = os.Getenv("HOME") + "/storage/music"
	}

	return model{
		client:            c,
		cursor:            0,
		musicDir:          musicPath,
		clockOffset:       ntpOffset + hardwareLatency,
		ntpStatus:         ntpStatusMsg,
		cursorInitialized: false,
		syncCooldownUntil: time.Now(),
		termHeight:        0, // Nastaví se dynamicky při prvním renderu
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchPlaylist(m.client), syncEngine(m.client))
}

// --- The Core State Machine (Update Loop) ---
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.termHeight = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.playlist)-1 {
				m.cursor++
			}

		case "enter":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
				ensureOutputsEnabled(m.client)
				_ = m.client.Play(m.cursor)
				m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
			}

		case "o":
			ensureOutputsEnabled(m.client)
			m.ntpStatus = "Audio Outputs Re-enabled"
			return m, nil

		case "b", "r":
			onAir, err := toggleBroadcast(m.client)
			if err != nil {
				m.ntpStatus = fmt.Sprintf("Broadcast Error: %v", err)
			} else if onAir {
				m.ntpStatus = "🔴 BROADCAST: ON AIR (Icecast Active)"
			} else {
				m.ntpStatus = "⚪ BROADCAST: OFF AIR (Icecast Muted)"
			}
			return m, nil

		case "a":
			return m, runFzf(m.musicDir)

		case "d":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
				_ = m.client.Delete(m.cursor, -1)
				if m.cursor > 0 && m.cursor >= len(m.playlist)-1 {
					m.cursor--
				}
				return m, fetchPlaylist(m.client)
			}

		case "delete":
			if len(m.playlist) > 0 {
				_ = m.client.Clear()
				m.cursor = 0
				return m, fetchPlaylist(m.client)
			}

		case "+", "=":
			m.clockOffset += 100 * time.Millisecond
			m.ntpStatus = fmt.Sprintf("Manual Tuning Tweak: %.3fs", m.clockOffset.Seconds())
			return m, nil

		case "-":
			m.clockOffset -= 100 * time.Millisecond
			m.ntpStatus = fmt.Sprintf("Manual Tuning Tweak: %.3fs", m.clockOffset.Seconds())
			return m, nil

		case "pgup":
			if m.cursor > 0 {
				target := m.cursor - 1
				_ = m.client.Move(m.cursor, m.cursor+1, target)
				m.cursor = target
				return m, fetchPlaylist(m.client)
			}

		case "pgdown":
			if len(m.playlist) > 0 && m.cursor < len(m.playlist)-1 {
				target := m.cursor + 1
				_ = m.client.Move(m.cursor, m.cursor+1, target)
				m.cursor = target
				return m, fetchPlaylist(m.client)
			}
		}

	case playlistMsg:
		m.playlist = msg

	case fzfResultMsg:
		if len(msg) > 0 {
			ensureOutputsEnabled(m.client)
			for _, track := range msg {
				err := m.client.Add(track)
				if err != nil {
					// Fallback for unindexed/new files: update MPD DB and retry Add
					_, _ = m.client.Update(track)
					_ = m.client.Add(track)
				}
			}
			_ = os.Remove(os.Getenv("HOME") + "/observatory_fzf.txt")
			return m, fetchPlaylist(m.client)
		}

	case statusMsg:
		m.currentStatus = mpd.Attrs(msg)
		if errStr, ok := m.currentStatus["error"]; ok && errStr != "" {
			ensureOutputsEnabled(m.client)
		}
		currentSongID := m.currentStatus["songid"]
		songPos, _ := strconv.Atoi(m.currentStatus["song"])

		var totalTrackDuration float64
		if durStr, ok := m.currentStatus["duration"]; ok {
			totalTrackDuration, _ = strconv.ParseFloat(durStr, 64)
		}

		if !m.cursorInitialized {
			if currentSongID != "" {
				m.cursor = songPos
				m.lastSongID = currentSongID
				m.cursorInitialized = true
			}
		}

		if currentSongID != "" && currentSongID != m.lastSongID && m.playlist != nil {
			m.lastSongID = currentSongID

			trueTime := time.Now().Add(m.clockOffset)
			targetSec := float64(trueTime.Second()) + float64(trueTime.Nanosecond())/1e9

			if totalTrackDuration > 0 {
				targetSec = math.Mod(targetSec, totalTrackDuration)
				preciseSeekRaw(targetSec)
			} else {
				preciseSeekRaw(0.0)
			}

			m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
			return m, syncEngine(m.client)
		}

		if m.currentStatus["state"] == "play" && time.Now().After(m.syncCooldownUntil) {
			trueTime := time.Now().Add(m.clockOffset)
			targetSecondOfSystem := float64(trueTime.Second()) + float64(trueTime.Nanosecond())/1e9

			mpdElapsed, _ := strconv.ParseFloat(m.currentStatus["elapsed"], 64)
			trackSecond := math.Mod(mpdElapsed, 60)

			drift := targetSecondOfSystem - trackSecond
			if drift < -30 {
				drift += 60
			} else if drift > 30 {
				drift -= 60
			}

			if drift > 0.3 || drift < -0.3 {
				idealTrackPosition := mpdElapsed + drift

				if totalTrackDuration > 0 && idealTrackPosition >= (totalTrackDuration-2.0) {
					return m, syncEngine(m.client)
				}

				if idealTrackPosition < 0 {
					idealTrackPosition = 0.0
				}

				preciseSeekRaw(idealTrackPosition)
				m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
			}
		}

		return m, syncEngine(m.client)

	case errMsg:
		m.err = msg
		return m, nil
	}

	return m, nil
}

// --- Helper Functions ---
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// --- The UI Renderer (View Loop) ---
func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n  Error encountered: %v\n\n  Press 'q' to exit.", m.err)
	}

	var s strings.Builder
	airStatus := "\033[90m[ ⚪ OFF AIR ]\033[0m"
	if isBroadcastActive(m.client) {
		airStatus = "\033[1;31m[ 🔴 ON AIR ]\033[0m"
	}
	s.WriteString(fmt.Sprintf("\n // NTP TERMINAL MPD PLAYER %s // %s ///////////////////// \n\n", version, airStatus))

	currentSongIndex := -1
	if m.currentStatus != nil {
		if idx, err := strconv.Atoi(m.currentStatus["song"]); err == nil && m.currentStatus["state"] == "play" {
			currentSongIndex = idx
		}
	}

	if len(m.playlist) == 0 {
		s.WriteString("   (No tracks loaded. Press [a] to add music via FZF)\n")
	} else {
		// Dynamický výpočet velikosti stránky podle výšky terminálu
		// Rezerva 9 řádků je pro záhlaví, patičku, stavové řádky a stránkovací info
		pageSize := 10
		if m.termHeight > 9 {
			pageSize = m.termHeight - 9
		}

		// Výpočet indexů pro aktuální stránku tak, aby kurzor byl vždy viditelný
		startIdx := (m.cursor / pageSize) * pageSize
		endIdx := startIdx + pageSize
		if endIdx > len(m.playlist) {
			endIdx = len(m.playlist)
		}

		totalPages := int(math.Ceil(float64(len(m.playlist)) / float64(pageSize)))
		currentPage := (startIdx / pageSize) + 1

		// Vykreslení pouze songů na aktuální stránce
		for i := startIdx; i < endIdx; i++ {
			track := m.playlist[i]
			cursorStr := "  "
			if i == m.cursor {
				cursorStr = " > "
			}

			title := track["title"]
			if title == "" {
				file := track["file"]
				parts := strings.Split(file, "/")
				title = parts[len(parts)-1]
			}

			if i == currentSongIndex {
				s.WriteString(fmt.Sprintf("%s\033[32m%d. %s\033[0m\n", cursorStr, i+1, title))
			} else {
				s.WriteString(fmt.Sprintf("%s%d. %s\n", cursorStr, i+1, title))
			}
		}

		// Zobrazení navigační lišty stránek
		s.WriteString(fmt.Sprintf("\n  [ Strana %d / %d | Zobrazeno %d-%d z %d ]\n", currentPage, totalPages, startIdx+1, endIdx, len(m.playlist)))
	}

	s.WriteString("\n---------------------------------------------------------------\n")
	s.WriteString(fmt.Sprintf("  %s\n", m.ntpStatus))
	s.WriteString("  [↑/↓] Move | [Enter] Play | [a] Add | [b/r] On-Air Radio | [d] Del | [o] Audio Reset | [q] Quit\n")

	return s.String()
}

func main() {
	var mockNtpOffset time.Duration = 0 * time.Millisecond

	p := tea.NewProgram(initialModel(mockNtpOffset), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatal("Runtime panic within Bubble Tea environment:", err)
	}
}
