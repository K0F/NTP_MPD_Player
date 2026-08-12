# NTP Terminal MPD Player

A terminal UI (TUI) MPD client that keeps playback locked to the wall clock
via real NTP sync, with optional Icecast radio broadcasting, live listener
counting and track metadata pushing.

## Features

- **Real NTP clock sync** — queries `pool.ntp.org`, `time.cloudflare.com` and
  `time.google.com` on startup and uses the measured offset to keep the track
  position aligned with the actual wall-clock time.
- **Automatic drift correction** — while playing, the track position is
  continuously compared to the true time; any drift beyond 300 ms triggers a
  millisecond-precision `seekcur` correction (with a 2.5 s cooldown).
- **Manual tuning tweaks** — fine-tune the clock offset by +/-100 ms on the fly
  for hardware with extra latency (e.g. Android audio).
- **Now Playing panel** — shows the current playback state (`[ PLAY ]`,
  `[ PAUSE ]`, `[ STOP ]`), track title, artist/album and a live progress bar
  with elapsed/total time and percentage.
- **Live NTP clock display** — the footer always shows the NTP-corrected time
  and the applied offset.
- **Playlist columns** — tracks are laid out in aligned columns with the playing
  track marked `*` and highlighted green, the cursor row shown in reverse video,
  and durations right-aligned. Long titles are truncated to the terminal width.
- **Volume control** — `[` / `]` adjust MPD volume in 5% steps.
- **FZF track search** — launch `fzf` over the music directory (filtered to
  audio files) and add multiple tracks to the playlist in one go. If a file
  isn't in MPD's database yet, it is rescanned and added automatically.
- **Playlist management** — play/pause, next, stop, move, delete individual
  tracks, or clear the whole queue.
- **Paginated playlist view** — page-by-page track list that follows the cursor,
  with a page indicator.
- **On Air radio broadcast** — one-key toggle (`b` / `r`) to enable or mute the
  Icecast streaming output.
- **Live listener count** — polls Icecast's `status-json.xsl` every 10 s and
  shows the number of connected listeners in the header (`[ ON AIR: N ]`) and
  the footer.
- **Icecast metadata push** — on every song change the `artist - title` is sent
  to the Icecast `admin/metadata` endpoint so listeners see the current track.
- **Auto-recovery** — re-enables all disabled MPD audio outputs on play, on the
  `o` hotkey, and whenever MPD reports an error state.
- **Help overlay** — press `?` for a full keybinding reference.
- **Termux / Android support** — auto-detects Termux, switches the music
  directory to `~/storage/music` and applies a 450 ms hardware-audio latency
  profile.

## Interface

```
 // NTP TERMINAL MPD PLAYER v0.2 ////////////////////////// [ ON AIR: 3 ] //

 [ PLAY ]  Tropic of Cancer - Awake
           Tropic of Cancer  2024
 [#####----------------------------------------] 0:41 / 5:12  (13%)
 --------------------------------------------------------------------------
 > 1. * Tropic of Cancer - Awake                                   [5:12]
   2.   Some Artist - Some Title                                    [3:45]
 ...
   [ Page 1 / 3 | Shown 1-10 of 25 ]

   NTP (pool.ntp.org): offset +12ms
   Vol 80% | Clock 14:32:05 | Offset +12ms | Listeners 3
   [k/j] Move | [Enter] Play/Pause | [n] Next | [s] Sync | [a] Add | [b/r] Radio | [x] Stop | [?] Help | [q] Quit
```

## Keybindings

| Key            | Action                                          |
| -------------- | ----------------------------------------------- |
| `k` / `up`     | Move cursor up                                  |
| `j` / `down`   | Move cursor down                                |
| `Enter`        | Play / pause toggle (starts selected track)     |
| `n`            | Next track                                      |
| `s`            | Force wall-clock sync now                       |
| `x`            | Stop playback                                   |
| `[` / `]`      | Volume down / up (5% steps)                     |
| `a`            | Add music via FZF                               |
| `d`            | Delete selected track                           |
| `Delete`       | Clear the whole playlist                        |
| `PageUp`       | Move selected track up                          |
| `PageDown`     | Move selected track down                        |
| `b` / `r`      | Toggle On Air broadcast (Icecast output)        |
| `o`            | Re-enable all audio outputs                     |
| `+` / `=`      | Increase clock offset by +100 ms (tuning tweak) |
| `-`            | Decrease clock offset by -100 ms (tuning tweak) |
| `?`            | Toggle help overlay                             |
| `q` / `Ctrl+C` | Quit                                            |

## How it works

- **NTP sync** — on startup the clock offset relative to a pool NTP server is
  measured. Any hardware latency (e.g. 450 ms on Termux/Android) is added on top.
- **Wall-clock alignment** — each time a track starts, it is seeked to the
  current second-of-the-system so the "top of the minute" of every track matches
  the real minute boundary. While playing, small drifts are corrected
  continuously (with a 2.5 s cooldown to avoid seek fighting). `s` forces the
  alignment immediately.
- **Broadcasting** — the Icecast stream is a regular MPD output. Toggling
  `b`/`r` simply enables/disables that output. On song change, the metadata is
  pushed over HTTP with Basic Auth to Icecast's admin interface.
- **Listener counting** — every 10 s the public `status-json.xsl` endpoint is
  polled (no auth needed) and the listener count for the configured mount is
  shown in the header and footer. Both single-source and multi-source responses
  are handled.

## Configuration

Icecast connection details are hard-coded at the top of `main.go`:

| Setting   | Default          |
| --------- | ---------------- |
| Host      | `192.168.1.101`  |
| Port      | `9000`           |
| Mount     | `/radio.mp3`     |
| User      | `source`         |
| Password  | `ICECAST_PASSWORD` env var |
| MPD host  | `localhost:6600` |
| Music dir | read from `music_directory` in MPD config |

The music directory is read from the MPD config file (`music_directory`), not
hard-coded. Config is located via `$XDG_CONFIG_HOME/mpd/mpd.conf`,
`~/.config/mpd/mpd.conf` or `/etc/mpd.conf`, with `~/` expanded. Falls back to
`~/Music`, or `~/storage/music` on Termux, if MPD config is unavailable.

The Icecast source password is read from the `ICECAST_PASSWORD` environment
variable and is never stored in the source tree. Set it before running, e.g.:

```sh
ICECAST_PASSWORD=your-secret make run
```

## Build, Run, Test

```sh
make build   # builds ./mpdplayer
make run     # go run main.go
make test    # go test ./...
make fmt     # gofumpt -w .
make lint    # golangci-lint run
```

An integration test against a live MPD daemon can be run with:

```sh
MPD_INTEGRATION=1 go test -run Integration -v
```

It is skipped automatically unless `MPD_INTEGRATION=1` is set, MPD is
reachable on `localhost:6600`, and the queue is idle (so playback is never
disturbed).

Requires MPD running locally on `localhost:6600` and `fzf` for track search.
