package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type UIState struct {
	SidebarVisible bool              `json:"sidebar_visible"`
	UnreadOnly     bool              `json:"unread_only"`
	Favorites      []string          `json:"favorites"`
	ReadTimestamps map[string]string `json:"read_timestamps"`
}

func DefaultUIState() UIState {
	return UIState{
		SidebarVisible: false,
		UnreadOnly:     true,
		Favorites:      []string{},
		ReadTimestamps: make(map[string]string),
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
