# wlslack

A Wayland-native Slack GUI client written in Go, built on [Gio](https://gioui.org).

Based on the same Slack backend as [lazyslack](https://github.com/user/lazyslack) (the terminal client), exposed through a graphical UI suitable for sway, Hyprland, and other wlroots-based compositors.

## Features (MVP)

- Channel sidebar (public, private, IM, MPIM)
- Live message list with polling refresh
- Plain Enter sends, Shift+Enter inserts a newline
- Slack mrkdwn rendering: `*bold*`, `_italic_`, `~strike~`, inline / fenced code, mentions (`@user`, `@channel`, `@subteam`), channel links (`#name`), URL links, `:emoji:` shortcodes
- TOML config plus on-disk channel and message cache
- Single Go binary; no Electron, no XWayland required

## Build

```
go build -o wlslack ./cmd/wlslack
```

Or:

```
make build
```

## Run

```
SLACK_TOKEN=xoxp-... ./wlslack
```

Set `WAYLAND_DISPLAY` (sway, Hyprland, GNOME, etc. set this automatically) and Gio will use the native Wayland backend. The same binary also runs under X11 if no Wayland display is available.

## Configuration

Same shape as lazyslack's TOML config. Defaults are sensible; the file is optional.

Default location: `~/.config/wlslack/config.toml`
Override with `WLSLACK_CONFIG=/path/to/config.toml`.

```toml
[display]
message_limit   = 100
timestamp_format = "3:04 PM"

[polling]
active_channel = 3   # seconds
channel_list   = 300 # seconds

[channels]
pinned = ["C12345678"]
hidden = []
types  = ["public_channel", "private_channel", "im", "mpim"]
```

## Required Slack scopes

Use a user token (`xoxp-...`) with at least:

```
channels:read groups:read im:read mpim:read
channels:history groups:history im:history mpim:history
chat:write reactions:write users:read usergroups:read search:read
```

## Cache & logs

- `~/.cache/wlslack/channels.json` — channel list snapshot
- `~/.cache/wlslack/messages/*.json` — per-channel message history
- `~/.cache/wlslack/wlslack.log` — debug log (slog text format)

## Layout

```
cmd/wlslack/main.go       bootstrap (logger, config, slack client, UI)
internal/slack/           API wrapper, in-memory + disk cache, formatter
internal/config/          TOML config + UI state JSON
internal/logger/          slog file handler
internal/ui/              Gio UI (app, channels, messages, composer, theme)
```

## Status

MVP. Threads, reactions, search, mentions screen, file attachments, and avatar images are deliberately deferred — the slack package already exposes the necessary methods, so they are pure UI work.
