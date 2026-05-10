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
	ShowUnreadOnCollapse   bool              `json:"show_unread_on_collapse"`
	ShowStatusBar          bool              `json:"show_status_bar"`
	DisableLinkUnfurl      bool              `json:"disable_link_unfurl"`
	DisableMediaUnfurl     bool              `json:"disable_media_unfurl"`
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
	Global   FontPref `json:"global"`
	Channels FontPref `json:"channels"`
	Header   FontPref `json:"header"`
	Messages FontPref `json:"messages"`
	Threads  FontPref `json:"threads"`
	Composer FontPref `json:"composer"`
	Code     FontPref `json:"code"`
	Search   FontPref `json:"search"`
	UserInfo FontPref `json:"user_info"`
	StatusBar FontPref `json:"status_bar"`
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
		ShowUnreadOnCollapse:   true,
		ShowStatusBar:          true,
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

func LoadUIState() (UIState, bool) {
	state := DefaultUIState()
	data, err := os.ReadFile(statePath())
	if err != nil {
		return state, false
	}
	_ = json.Unmarshal(data, &state)
	return state, true
}

func SaveUIState(state UIState) {
	data, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(statePath(), data, 0600)
}
