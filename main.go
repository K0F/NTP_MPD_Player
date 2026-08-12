package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ntplib "github.com/beevik/ntp"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fhs/gompd/v2/mpd"
)

var version string = "0.3"

// --- Message Types ---
type (
	statusMsg       mpd.Attrs
	playlistMsg     []mpd.Attrs
	fzfResultMsg    []string
	icecastStatsMsg int
	errMsg          error
)

// --- Application Model ---
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
	syncCooldownUntil time.Time
	termHeight        int
	termWidth         int
	showHelp          bool
	listeners         int
	volume            int
}

// --- Icecast Metadata Push ---

// The Icecast source password comes from the environment so it never has to
// live in the source tree.
var icecastCfg = struct {
	host     string
	port     string
	mount    string
	password string
	user     string
}{
	host:     "192.168.1.101",
	port:     "9000",
	mount:    "/radio.mp3",
	password: os.Getenv("ICECAST_PASSWORD"),
	user:     "source",
}

func pushIcecastMeta(artist, title string) {
	go func() {
		song := title
		if artist != "" {
			song = artist + " - " + title
		}

		endpoint := fmt.Sprintf("http://%s:%s/admin/metadata", icecastCfg.host, icecastCfg.port)
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return
		}

		q := url.Values{}
		q.Set("mount", icecastCfg.mount)
		q.Set("mode", "updinfo")
		q.Set("song", song)
		req.URL.RawQuery = q.Encode()
		req.SetBasicAuth(icecastCfg.user, icecastCfg.password)

		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// parseIcecastListeners extracts the listener count for mount from the
// status-json.xsl response. source may be a single object or an array.
func parseIcecastListeners(body []byte, mount string) (int, error) {
	var resp struct {
		Ices struct {
			Source json.RawMessage `json:"source"`
		} `json:"icestats"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	if len(resp.Ices.Source) == 0 {
		return 0, nil
	}

	type source struct {
		Listeners int    `json:"listeners"`
		ListenURL string `json:"listenurl"`
	}

	var sources []source
	if resp.Ices.Source[0] == '[' {
		if err := json.Unmarshal(resp.Ices.Source, &sources); err != nil {
			return 0, err
		}
	} else {
		var single source
		if err := json.Unmarshal(resp.Ices.Source, &single); err != nil {
			return 0, err
		}
		sources = []source{single}
	}

	if len(sources) == 0 {
		return 0, nil
	}
	for _, s := range sources {
		if mount != "" && strings.Contains(s.ListenURL, mount) {
			return s.Listeners, nil
		}
	}
	return sources[0].Listeners, nil
}

// pollIcecastListeners returns the current radio listener count, or -1 when the
// stats endpoint is unreachable.
func pollIcecastListeners() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(10 * time.Second)
		endpoint := fmt.Sprintf("http://%s:%s/status-json.xsl", icecastCfg.host, icecastCfg.port)
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(endpoint)
		if err != nil {
			return icecastStatsMsg(-1)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return icecastStatsMsg(-1)
		}
		count, err := parseIcecastListeners(body, icecastCfg.mount)
		if err != nil {
			return icecastStatsMsg(-1)
		}
		return icecastStatsMsg(count)
	}
}

// --- Audio Control Helpers ---
func preciseSeekRaw(targetSec float64) {
	conn, err := net.Dial("tcp", "localhost:6600")
	if err != nil {
		return
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	if _, err := conn.Read(buf); err != nil {
		return
	}

	cmd := fmt.Sprintf("seekcur %.3f\n", targetSec)
	_, _ = conn.Write([]byte(cmd))
}

// wallClockTargetSec computes the track position that aligns playback with the
// wall clock (second-of-minute), wrapping around the track duration. Returns
// false when no playable duration is known.
func wallClockTargetSec(clockOffset time.Duration, duration string) (float64, bool) {
	total, err := strconv.ParseFloat(duration, 64)
	if err != nil || total <= 0 {
		return 0, false
	}
	trueTime := time.Now().Add(clockOffset)
	targetSec := float64(trueTime.Second()) + float64(trueTime.Nanosecond())/1e9
	return math.Mod(targetSec, total), true
}

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

func toggleBroadcast(client *mpd.Client) (bool, error) {
	outputs, err := client.ListOutputs()
	if err != nil {
		return false, err
	}
	var targetID int = -1
	var currentlyEnabled bool = false

	for _, out := range outputs {
		idStr, hasID := out["outputid"]
		name := out["outputname"]
		enabled := out["outputenabled"]

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
				enabled := outputs[1]["outputenabled"]
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

func isBroadcastActive(client *mpd.Client) bool {
	outputs, err := client.ListOutputs()
	if err != nil {
		return false
	}
	for _, out := range outputs {
		idStr := out["outputid"]
		name := out["outputname"]
		enabled := out["outputenabled"]

		if strings.Contains(strings.ToLower(name), "icecast") || strings.Contains(strings.ToLower(name), "stream") || idStr == "1" {
			return enabled == "1" || enabled == "true"
		}
	}
	return false
}

func renderHeader(version string, onAir bool, listeners int, width int) string {
	left := fmt.Sprintf(" // NTP TERMINAL MPD PLAYER v%s ", version)
	statusText := "[ OFF AIR ]"
	statusFormatted := "\033[90m[ OFF AIR ]\033[0m"
	if onAir {
		if listeners >= 0 {
			statusText = fmt.Sprintf("[ ON AIR: %d ]", listeners)
		} else {
			statusText = "[ ON AIR: ? ]"
		}
		statusFormatted = "\033[1;31m" + statusText + "\033[0m"
	}
	rightLen := len(statusText) + 4 // trailing " %s //" after fill

	fillLen := width - len(left) - rightLen
	if fillLen < 2 {
		fillLen = 2
	}
	fill := strings.Repeat("/", fillLen)

	return fmt.Sprintf("\n%s%s %s //\n\n", left, fill, statusFormatted)
}

// --- Background Polling Commands ---
func syncEngine(client *mpd.Client) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(500 * time.Millisecond)
		status, err := client.Status()
		if err != nil {
			return errMsg(err)
		}
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

// addTrackToPlaylist adds uri to the playlist. If MPD does not know the file
// yet, it triggers a database rescan and retries until the file is indexed, so
// files that just appeared on disk get added. MPD queues update jobs, so we
// keep retrying rather than relying on job IDs.
func addTrackToPlaylist(client *mpd.Client, uri string) error {
	if err := client.Add(uri); err == nil {
		return nil
	}
	if _, err := client.Update(uri); err != nil {
		return err
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Add(uri); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("file not indexed after rescan: %s", uri)
}

// --- FZF Integration ---

var audioExts = []string{
	"mp3", "flac", "wav", "opus", "ogg", "oga",
	"m4a", "m4b", "aac", "mp4", "webm", "mka",
	"wma", "aiff", "aif", "ape", "wv", "dsf", "dff", "alac",
}

func findAudioExpr() string {
	parts := make([]string, 0, len(audioExts))
	for _, ext := range audioExts {
		parts = append(parts, "-iname '*."+ext+"'")
	}
	return strings.Join(parts, " -o ")
}

func runFzf(musicDir string) tea.Cmd {
	return tea.ExecProcess(exec.Command("sh", "-c", fmt.Sprintf(
		"cd %q && find . -type f \\( %s \\) -not -path '*/.*' | fzf -m > $HOME/observatory_fzf.txt",
		musicDir, findAudioExpr(),
	)), func(err error) tea.Msg {
		if err != nil {
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

// --- MPD Config Parsing ---

func expandPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func parseMusicDir(config string) string {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "music_directory") {
			continue
		}
		rest := strings.TrimSpace(line[len("music_directory"):])
		if rest == "" {
			continue
		}
		if strings.HasPrefix(rest, `"`) {
			if end := strings.Index(rest[1:], `"`); end != -1 {
				return rest[1 : 1+end]
			}
		}
		return rest
	}
	return ""
}

func mpdMusicDir() string {
	home, _ := os.UserHomeDir()
	var configPaths []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		configPaths = append(configPaths, filepath.Join(xdg, "mpd", "mpd.conf"))
	}
	if home != "" {
		configPaths = append(configPaths, filepath.Join(home, ".config", "mpd", "mpd.conf"))
	}
	configPaths = append(configPaths, "/etc/mpd.conf")

	for _, path := range configPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if dir := parseMusicDir(string(content)); dir != "" {
			return expandPath(dir)
		}
	}
	return ""
}

// --- Model Initialization ---
func initialModel(ntpOffset time.Duration, ntpMsg string) model {
	c, err := mpd.Dial("tcp", "localhost:6600")
	if err != nil {
		log.Fatal("Could not connect to MPD local daemon:", err)
	}

	musicPath := mpdMusicDir()
	var hardwareLatency time.Duration = 0 * time.Millisecond

	if os.Getenv("TERMUX_VERSION") != "" {
		hardwareLatency = 450 * time.Millisecond
		ntpMsg = "NTP + Android Hardware Audio Profile Active (+0.450s)"
		if musicPath == "" {
			musicPath = os.Getenv("HOME") + "/storage/music"
		}
	}

	if musicPath == "" {
		musicPath = "~/Music"
		musicPath = expandPath(musicPath)
	}

	return model{
		client:            c,
		cursor:            0,
		musicDir:          musicPath,
		clockOffset:       ntpOffset + hardwareLatency,
		ntpStatus:         ntpMsg,
		cursorInitialized: false,
		syncCooldownUntil: time.Now(),
		termHeight:        0,
		termWidth:         84,
		showHelp:          false,
		listeners:         -1,
		volume:            -1,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(fetchPlaylist(m.client), syncEngine(m.client), pollIcecastListeners())
}

// --- State Update Loop ---
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.termHeight = msg.Height
		m.termWidth = msg.Width
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
			ensureOutputsEnabled(m.client)
			st, err := m.client.Status()
			if err != nil {
				m.ntpStatus = fmt.Sprintf("Status error: %v", err)
				return m, nil
			}
			switch st["state"] {
			case "play":
				_ = m.client.Pause(true)
				m.ntpStatus = "Paused"
			case "pause":
				_ = m.client.Pause(false)
				m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
				m.ntpStatus = "Resumed"
			default:
				if len(m.playlist) > 0 && m.cursor < len(m.playlist) {
					_ = m.client.Play(m.cursor)
					m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
					m.ntpStatus = "Playing"
				}
			}
			return m, nil

		case "n":
			ensureOutputsEnabled(m.client)
			if err := m.client.Next(); err != nil {
				m.ntpStatus = fmt.Sprintf("Next error: %v", err)
			} else {
				m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
				m.ntpStatus = "Next track"
			}
			return m, nil

		case "s":
			ensureOutputsEnabled(m.client)
			if targetSec, ok := wallClockTargetSec(m.clockOffset, m.currentStatus["duration"]); ok {
				preciseSeekRaw(targetSec)
				m.syncCooldownUntil = time.Now().Add(2500 * time.Millisecond)
				m.ntpStatus = "Forced wall-clock sync"
			} else {
				m.ntpStatus = "Sync: no track playing"
			}
			return m, nil

		case "x":
			_ = m.client.Stop()
			m.ntpStatus = "Stopped"
			return m, nil

		case "[":
			if m.volume < 0 {
				if st, err := m.client.Status(); err == nil {
					m.volume, _ = strconv.Atoi(st["volume"])
				}
			}
			vol := m.volume - 5
			if vol < 0 {
				vol = 0
			}
			_ = m.client.SetVolume(vol)
			m.volume = vol
			m.ntpStatus = fmt.Sprintf("Volume %d%%", vol)
			return m, nil

		case "]":
			if m.volume < 0 {
				if st, err := m.client.Status(); err == nil {
					m.volume, _ = strconv.Atoi(st["volume"])
				}
			}
			vol := m.volume + 5
			if vol > 100 {
				vol = 100
			}
			_ = m.client.SetVolume(vol)
			m.volume = vol
			m.ntpStatus = fmt.Sprintf("Volume %d%%", vol)
			return m, nil

		case "?":
			m.showHelp = !m.showHelp
			return m, nil

		case "o":
			ensureOutputsEnabled(m.client)
			m.ntpStatus = "Audio Outputs Re-enabled"
			return m, nil

		case "b", "r":
			onAir, err := toggleBroadcast(m.client)
			if err != nil {
				m.ntpStatus = fmt.Sprintf("Broadcast Error: %v", err)
			} else if onAir {
				m.ntpStatus = "[ON AIR] Broadcast Active (Icecast)"
			} else {
				m.ntpStatus = "[OFF AIR] Broadcast Muted (Icecast)"
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
			var failed []string
			for _, track := range msg {
				if err := addTrackToPlaylist(m.client, track); err != nil {
					failed = append(failed, track)
				}
			}
			if len(failed) > 0 {
				m.ntpStatus = fmt.Sprintf("Add failed (%d): %s", len(failed), strings.Join(failed, ", "))
			} else {
				m.ntpStatus = fmt.Sprintf("Added %d track(s)", len(msg))
			}
			_ = os.Remove(os.Getenv("HOME") + "/observatory_fzf.txt")
			return m, fetchPlaylist(m.client)
		}

	case statusMsg:
		m.currentStatus = mpd.Attrs(msg)
		if v, err := strconv.Atoi(m.currentStatus["volume"]); err == nil {
			m.volume = v
		}
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

			if songPos >= 0 && songPos < len(m.playlist) {
				track := m.playlist[songPos]
				artist := track["artist"]
				title := track["title"]
				if title == "" {
					parts := strings.Split(track["file"], "/")
					title = parts[len(parts)-1]
				}
				pushIcecastMeta(artist, title)
			}

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

	case icecastStatsMsg:
		m.listeners = int(msg)
		return m, pollIcecastListeners()

	case errMsg:
		m.err = msg
		return m, nil
	}

	return m, nil
}

// --- View Renderer ---

func truncate(s string, max int) string {
	if max < 0 {
		max = 0
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		if max == 0 {
			return ""
		}
		return string(r[:1])
	}
	return string(r[:max-1]) + "~"
}

func formatDur(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h, m, s := total/3600, (total%3600)/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func wallClock(offset time.Duration) string {
	return time.Now().Add(offset).Format("15:04:05")
}

func formatOffset(d time.Duration) string {
	ms := d.Milliseconds()
	sign := "+"
	if ms < 0 {
		sign = "-"
		ms = -ms
	}
	return fmt.Sprintf("%s%dms", sign, ms)
}

func trackTitle(track mpd.Attrs) string {
	if title := track["title"]; title != "" {
		if artist := track["artist"]; artist != "" {
			return artist + " - " + title
		}
		return title
	}
	parts := strings.Split(track["file"], "/")
	return parts[len(parts)-1]
}

// renderTrackRow lays out one playlist line, keeping the row exactly width
// characters wide.
func renderTrackRow(track mpd.Attrs, num, total, cursor int, playing bool, width int) string {
	index := fmt.Sprintf("%*d. ", len(strconv.Itoa(total)), num)
	marker := " "
	if playing {
		marker = "*"
	}
	prefix := "   " + index + marker + " "
	if num-1 == cursor {
		prefix = " > " + index + marker + " "
	}

	dur := ""
	if d, err := strconv.ParseFloat(track["duration"], 64); err == nil && d > 0 {
		dur = "[" + formatDur(d) + "]"
	}

	titleBudget := width - len(prefix) - len(dur)
	if titleBudget < 1 {
		titleBudget = 1
	}
	title := truncate(trackTitle(track), titleBudget-1)
	pad := width - len(prefix) - len(title) - len(dur)

	row := prefix + title + strings.Repeat(" ", pad) + dur
	switch {
	case playing && num-1 == cursor:
		return "\033[32m\033[7m" + row + "\033[0m"
	case playing:
		return "\033[32m" + row + "\033[0m"
	case num-1 == cursor:
		return "\033[7m" + row + "\033[0m"
	default:
		return row
	}
}

func renderNowPlaying(m model, width int) string {
	var s strings.Builder
	state := m.currentStatus["state"]
	stateLabel, stateColor := "[ STOP ]", "\033[90m"
	switch state {
	case "play":
		stateLabel, stateColor = "[ PLAY ]", "\033[32m"
	case "pause":
		stateLabel, stateColor = "[ PAUSE ]", "\033[33m"
	}

	title, artist, album := "", "", ""
	if pos, err := strconv.Atoi(m.currentStatus["song"]); err == nil && pos >= 0 && pos < len(m.playlist) {
		t := m.playlist[pos]
		artist, title, album = t["artist"], t["title"], t["album"]
	}
	if title == "" {
		title = "(no track selected)"
	}

	line1 := stateColor + stateLabel + "\033[0m  " + truncate(title, width-len(stateLabel)-2)
	s.WriteString(line1 + "\n")

	if meta := strings.TrimSpace(artist + "  " + album); meta != "" {
		s.WriteString("  " + truncate(meta, width-2) + "\n")
	}

	if elapsed, err := strconv.ParseFloat(m.currentStatus["elapsed"], 64); err == nil {
		duration, _ := strconv.ParseFloat(m.currentStatus["duration"], 64)
		barWidth := width - 28
		if barWidth < 10 {
			barWidth = 10
		}
		if prog := renderProgressBar(elapsed, duration, barWidth); prog != "" {
			frac := 0.0
			if duration > 0 {
				frac = elapsed / duration
				if frac < 0 {
					frac = 0
				}
				if frac > 1 {
					frac = 1
				}
			}
			s.WriteString("  " + prog + fmt.Sprintf("  (%d%%)\n", int(frac*100)))
		}
	}
	return s.String()
}

func renderHelp(width int) string {
	rows := []struct{ key, desc string }{
		{"k/j, up/down", "move cursor"},
		{"enter", "play / pause toggle"},
		{"n", "next track"},
		{"s", "sync to wall clock"},
		{"x", "stop"},
		{"[ / ]", "volume down / up"},
		{"+ / -", "tune clock offset"},
		{"a", "add tracks via FZF"},
		{"d / delete", "delete track / clear queue"},
		{"pgup / pgdown", "move track in queue"},
		{"b / r", "toggle On Air radio"},
		{"o", "re-enable audio outputs"},
		{"? / q", "toggle help / quit"},
	}
	var s strings.Builder
	s.WriteString("\n  KEYBINDINGS\n  " + strings.Repeat("-", width-2) + "\n")
	for _, r := range rows {
		s.WriteString(fmt.Sprintf("  %-22s %s\n", r.key, r.desc))
	}
	return s.String()
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\n  Error encountered: %v\n\n  Press 'q' to exit.", m.err)
	}

	width := 84
	if m.termWidth > 84 {
		width = m.termWidth
	}

	onAir := isBroadcastActive(m.client)

	var s strings.Builder
	s.WriteString(renderHeader(version, onAir, m.listeners, width))
	s.WriteString(renderNowPlaying(m, width))
	s.WriteString(strings.Repeat("-", width) + "\n")

	if m.showHelp {
		s.WriteString(renderHelp(width))
	} else if len(m.playlist) == 0 {
		s.WriteString("   (No tracks loaded. Press [a] to add music via FZF)\n")
	} else {
		pageSize := m.termHeight - strings.Count(s.String(), "\n") - 4
		if pageSize < 5 {
			pageSize = 5
		}

		startIdx := (m.cursor / pageSize) * pageSize
		endIdx := startIdx + pageSize
		if endIdx > len(m.playlist) {
			endIdx = len(m.playlist)
		}

		currentSongPos := -1
		if pos, err := strconv.Atoi(m.currentStatus["song"]); err == nil && m.currentStatus["state"] == "play" {
			currentSongPos = pos
		}

		for i := startIdx; i < endIdx; i++ {
			s.WriteString(renderTrackRow(m.playlist[i], i+1, len(m.playlist), m.cursor, i == currentSongPos, width) + "\n")
		}

		totalPages := int(math.Ceil(float64(len(m.playlist)) / float64(pageSize)))
		currentPage := (startIdx / pageSize) + 1
		s.WriteString(fmt.Sprintf("\n  [ Page %d / %d | Shown %d-%d of %d ]\n", currentPage, totalPages, startIdx+1, endIdx, len(m.playlist)))
	}

	listenerTxt := "?"
	if m.listeners >= 0 {
		listenerTxt = strconv.Itoa(m.listeners)
	}
	info := []string{
		"Clock " + wallClock(m.clockOffset),
		"Offset " + formatOffset(m.clockOffset),
		fmt.Sprintf("Listeners %s", listenerTxt),
	}
	if m.volume >= 0 {
		info = append([]string{fmt.Sprintf("Vol %d%%", m.volume)}, info...)
	}

	s.WriteString("\n  " + m.ntpStatus + "\n")
	s.WriteString("  " + strings.Join(info, " | ") + "\n")
	s.WriteString("  [k/j] Move | [Enter] Play/Pause | [n] Next | [s] Sync | [a] Add | [b/r] Radio | [x] Stop | [?] Help | [q] Quit\n")

	return s.String()
}

func queryNTP() (time.Duration, string) {
	servers := []string{
		"pool.ntp.org",
		"time.cloudflare.com",
		"time.google.com",
	}
	for _, srv := range servers {
		resp, err := ntplib.Query(srv)
		if err == nil {
			offset := resp.ClockOffset
			sign := "+"
			if offset < 0 {
				sign = "-"
				offset = -offset
			}
			msg := fmt.Sprintf("NTP (%s): offset %s%dms", srv, sign, offset.Milliseconds())
			return resp.ClockOffset, msg
		}
	}
	return 0, "NTP: sync failed, using system clock"
}

func renderProgressBar(elapsed, total float64, width int) string {
	if total <= 0 {
		return ""
	}
	frac := elapsed / total
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(math.Round(frac * float64(width)))
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)

	elSec := int(elapsed)
	totSec := int(total)
	if elSec < 0 {
		elSec = 0
	}
	return fmt.Sprintf(
		"[%s] %d:%02d / %d:%02d",
		bar,
		elSec/60, elSec%60,
		totSec/60, totSec%60,
	)
}

func main() {
	ntpOffset, ntpMsg := queryNTP()

	p := tea.NewProgram(initialModel(ntpOffset, ntpMsg), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatal("Runtime panic within Bubble Tea environment:", err)
	}
}
