# NTP Terminal MPD Player

A terminal UI (TUI) MPD client that keeps playback locked to the wall clock
via real NTP sync, with optional Icecast radio broadcasting and track metadata
pushing.

## Features

- **Real NTP clock sync** — queries `pool.ntp.org`, `time.cloudflare.com` and
  `time.google.com` on startup and uses the measured offset to keep the track
  position aligned with the actual wall-clock time.
- **Automatic drift correction** — while playing, the track position is
  continuously compared to the true time; any drift beyond 300 ms triggers a
  millisecond-precision `seekcur` correction.
- **Manual tuning tweaks** — fine-tune the clock offset by +/-100 ms on the fly
  for hardware with extra latency (e.g. Android audio).
- **Termux / Android support** — auto-detects Termux, switches the music
  directory to `~/storage/music` and applies a 450 ms hardware-audio latency
  profile.
- **FZF track search** — launch `fzf` over the music directory (filtered to
  audio files) and add multiple tracks to the playlist in one go. If a file
  isn't in MPD's database yet, it is rescanned and added automatically.
- **Playlist management** — play, move, delete individual tracks, or clear the
  whole queue.
- **Paginated playlist view** — page-by-page track list that follows the cursor,
  with the currently playing track highlighted in green.
- **Progress bar** — live ASCII progress bar with elapsed / total time.
- **On Air radio broadcast** — one-key toggle (`b` / `r`) to enable or mute the
  Icecast streaming output.
- **Icecast metadata push** — on every song change the `artist - title` is sent
  to the Icecast `admin/metadata` endpoint so listeners see the current track.
- **Auto-recovery** — re-enables all disabled MPD audio outputs on play, on the
  `o` hotkey, and whenever MPD reports an error state.
- **Live On Air header** — animated-style header banner showing `[ ON AIR ]` in
  red when broadcasting, `[ OFF AIR ]` dimmed otherwise.

## Keybindings

| Key            | Action                                          |
| -------------- | ----------------------------------------------- |
| `k` / `up`     | Move cursor up                                  |
| `j` / `down`   | Move cursor down                                |
| `Enter`        | Play selected track                             |
| `a`            | Add music via FZF                               |
| `d`            | Delete selected track                           |
| `Delete`       | Clear the whole playlist                        |
| `PageUp`       | Move selected track up                          |
| `PageDown`     | Move selected track down                        |
| `b` / `r`      | Toggle On Air broadcast (Icecast output)        |
| `o`            | Re-enable all audio outputs                     |
| `+` / `=`      | Increase clock offset by +100 ms (tuning tweak) |
| `-`            | Decrease clock offset by -100 ms (tuning tweak) |
| `q` / `Ctrl+C` | Quit                                            |

## How it works

- **NTP sync** — on startup the clock offset relative to a pool NTP server is
  measured. Any hardware latency (e.g. 450 ms on Termux/Android) is added on top.
- **Wall-clock alignment** — each time a track starts, it is seeked to the
  current second-of-the-system so the "top of the minute" of every track matches
  the real minute boundary. While playing, small drifts are corrected
  continuously (with a 2.5 s cooldown to avoid seek fighting).
- **Broadcasting** — the Icecast stream is a regular MPD output. Toggling
  `b`/`r` simply enables/disables that output. On song change, the metadata is
  pushed over HTTP with Basic Auth to Icecast's admin interface.

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

Requires MPD running locally on `localhost:6600`.
