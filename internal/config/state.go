package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type UIState struct {
	SidebarVisible         bool              `json:"sidebar_visible"`
	UnreadOnly             bool              `json:"unread_only"`
	ShowOnlyRecentChannels bool              `json:"show_only_recent_channels"`
	HideEmptyChannels      bool              `json:"hide_empty_channels"`
	Favorites              []string          `json:"favorites"`
	CollapsedGroups        []string          `json:"collapsed_groups"`
	ReadTimestamps         map[string]string `json:"read_timestamps"`
	Fonts                  FontPrefs         `json:"fonts"`
	ThemeSidebar           string            `json:"theme_sidebar"`
	ThemeMain              string            `json:"theme_main"`
}

// FontPrefs holds per-section typeface and size overrides. Zero values mean
// "use the theme default" so the file stays human-editable.
type FontPrefs struct {
	Channels FontPref `json:"channels"`
	Header   FontPref `json:"header"`
	Messages FontPref `json:"messages"`
	Threads  FontPref `json:"threads"`
	Composer FontPref `json:"composer"`
	Code     FontPref `json:"code"`
	Search   FontPref `json:"search"`
	UserInfo FontPref `json:"user_info"`
}

type FontPref struct {
	Face string  `json:"face"`
	Size float32 `json:"size"`
}

func DefaultUIState() UIState {
	return UIState{
		SidebarVisible:  false,
		UnreadOnly:             true,
		ShowOnlyRecentChannels: false,
		HideEmptyChannels:      false,
		Favorites:              []string{},
		CollapsedGroups: []string{},
		ReadTimestamps:  make(map[string]string),
		ThemeSidebar:    "dark",
		ThemeMain:       "dark",
	}
}

func statePath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = os.TempDir()
	}
	dir := filepath.Join(configDir, "wlslack")
	os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "state.json")
}

func LoadUIState() UIState {
	state := DefaultUIState()
	data, err := os.ReadFile(statePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(data, &state)
	return state
}

func SaveUIState(state UIState) {
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(statePath(), data, 0600)
}
