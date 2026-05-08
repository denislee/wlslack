package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Token  string
	Cookie string // optional: the `d` cookie value for xoxc tokens
	TeamID string

	Polling  PollConfig
	Display  DisplayConfig
	Channels ChannelConfig
}

type PollConfig struct {
	ActiveChannel  time.Duration
	ChannelList    time.Duration
	Priority       time.Duration
	Thread         time.Duration
	Presence       time.Duration
	IdleMultiplier int
}

type DisplayConfig struct {
	TimestampFormat    string
	DateSeparator      bool
	MessageLimit       int
	ShowBotMessages    bool
	ShowStatusBar      bool
	EmojiStyle         string
	Theme              string
	DisableLinkUnfurl  bool
	DisableMediaUnfurl bool
}

type ChannelConfig struct {
	Pinned []string
	Hidden []string
	Types  []string
}

type fileConfig struct {
	Polling  filePollConfig `toml:"polling"`
	Display  fileDisplay    `toml:"display"`
	Channels fileChannels   `toml:"channels"`
}

type filePollConfig struct {
	ActiveChannel  int `toml:"active_channel"`
	ChannelList    int `toml:"channel_list"`
	Priority       int `toml:"priority"`
	Thread         int `toml:"thread"`
	Presence       int `toml:"presence"`
	IdleMultiplier int `toml:"idle_multiplier"`
}

type fileDisplay struct {
	TimestampFormat    string `toml:"timestamp_format"`
	DateSeparator      *bool  `toml:"date_separator"`
	MessageLimit       int    `toml:"message_limit"`
	ShowBotMessages    *bool  `toml:"show_bot_messages"`
	ShowStatusBar      *bool  `toml:"show_status_bar"`
	EmojiStyle         string `toml:"emoji_style"`
	Theme              string `toml:"theme"`
	DisableLinkUnfurl  *bool  `toml:"disable_link_unfurl"`
	DisableMediaUnfurl *bool  `toml:"disable_media_unfurl"`
}

type fileChannels struct {
	Pinned []string `toml:"pinned"`
	Hidden []string `toml:"hidden"`
	Types  []string `toml:"types"`
}

func defaults() Config {
	return Config{
		Polling: PollConfig{
			ActiveChannel:  3 * time.Second,
			ChannelList:    15 * time.Second,
			Priority:       5 * time.Second,
			Thread:         5 * time.Second,
			Presence:       30 * time.Second,
			IdleMultiplier: 2,
		},
		Display: DisplayConfig{
			TimestampFormat:    "3:04 PM",
			DateSeparator:      true,
			MessageLimit:       50,
			ShowBotMessages:    true,
			ShowStatusBar:      true,
			EmojiStyle:         "unicode",
			Theme:              "default",
			DisableLinkUnfurl:  false,
			DisableMediaUnfurl: false,
		},
		Channels: ChannelConfig{
			Types: []string{"public_channel", "private_channel", "im", "mpim"},
		},
	}
}

func Load() (*Config, error) {
	cfg := defaults()

	cfg.Token = os.Getenv("SLACK_TOKEN")
	if cfg.Token == "" {
		return nil, fmt.Errorf("SLACK_TOKEN environment variable is required")
	}
	// xoxc tokens need the workspace's `d` cookie attached on every request;
	// xoxb/xoxp tokens don't, and leaving this empty is fine for those.
	cfg.Cookie = os.Getenv("SLACK_COOKIE")
	if strings.HasPrefix(cfg.Token, "xoxc-") && cfg.Cookie == "" {
		return nil, fmt.Errorf(
			"SLACK_TOKEN is an xoxc browser-session token but SLACK_COOKIE is empty.\n" +
				"  xoxc tokens are paired with the workspace's `d` cookie — without it, files.slack.com\n" +
				"  rejects file downloads. Extract the cookie from your browser:\n" +
				"    1. Open https://app.slack.com in a logged-in tab.\n" +
				"    2. DevTools → Application/Storage → Cookies → https://app.slack.com\n" +
				"    3. Copy the value of the `d` cookie (starts with xoxd-…), then\n" +
				"       export SLACK_COOKIE=xoxd-...")
	}
	cfg.TeamID = os.Getenv("SLACK_TEAM")

	configPath := os.Getenv("WLSLACK_CONFIG")
	if configPath == "" {
		configDir, err := os.UserConfigDir()
		if err == nil {
			configPath = filepath.Join(configDir, "wlslack", "config.toml")
		}
	}

	if configPath != "" {
		loadFromFile(&cfg, configPath)
	}

	return &cfg, nil
}

func loadFromFile(cfg *Config, path string) {
	var fc fileConfig
	_, err := toml.DecodeFile(path, &fc)
	if err != nil {
		return
	}

	if fc.Polling.ActiveChannel > 0 {
		cfg.Polling.ActiveChannel = time.Duration(fc.Polling.ActiveChannel) * time.Second
	}
	if fc.Polling.ChannelList > 0 {
		cfg.Polling.ChannelList = time.Duration(fc.Polling.ChannelList) * time.Second
	}
	if fc.Polling.Priority > 0 {
		cfg.Polling.Priority = time.Duration(fc.Polling.Priority) * time.Second
	}
	if fc.Polling.Thread > 0 {
		cfg.Polling.Thread = time.Duration(fc.Polling.Thread) * time.Second
	}
	if fc.Polling.Presence > 0 {
		cfg.Polling.Presence = time.Duration(fc.Polling.Presence) * time.Second
	}
	if fc.Polling.IdleMultiplier > 0 {
		cfg.Polling.IdleMultiplier = fc.Polling.IdleMultiplier
	}

	if fc.Display.TimestampFormat != "" {
		cfg.Display.TimestampFormat = fc.Display.TimestampFormat
	}
	if fc.Display.DateSeparator != nil {
		cfg.Display.DateSeparator = *fc.Display.DateSeparator
	}
	if fc.Display.MessageLimit > 0 {
		cfg.Display.MessageLimit = fc.Display.MessageLimit
	}
	if fc.Display.ShowBotMessages != nil {
		cfg.Display.ShowBotMessages = *fc.Display.ShowBotMessages
	}
	if fc.Display.ShowStatusBar != nil {
		cfg.Display.ShowStatusBar = *fc.Display.ShowStatusBar
	}
	if fc.Display.EmojiStyle != "" {
		cfg.Display.EmojiStyle = fc.Display.EmojiStyle
	}
	if fc.Display.Theme != "" {
		cfg.Display.Theme = fc.Display.Theme
	}
	if fc.Display.DisableLinkUnfurl != nil {
		cfg.Display.DisableLinkUnfurl = *fc.Display.DisableLinkUnfurl
	}
	if fc.Display.DisableMediaUnfurl != nil {
		cfg.Display.DisableMediaUnfurl = *fc.Display.DisableMediaUnfurl
	}

	if len(fc.Channels.Pinned) > 0 {
		cfg.Channels.Pinned = fc.Channels.Pinned
	}
	if len(fc.Channels.Hidden) > 0 {
		cfg.Channels.Hidden = fc.Channels.Hidden
	}
	if len(fc.Channels.Types) > 0 {
		cfg.Channels.Types = fc.Channels.Types
	}
}
