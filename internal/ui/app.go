package ui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/transfer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/config"
	"github.com/user/wlslack/internal/llm"
	"github.com/user/wlslack/internal/slack"
	"github.com/user/wlslack/internal/translate"
)

// focusPane identifies which pane vim-style nav targets.
type focusPane int

const (
	paneChannels focusPane = iota
	paneMessages
)

// App is the top-level Gio program.
type App struct {
	w      *app.Window
	th     *Theme
	client *slack.Client
	cfg    *config.Config
	fmt    *slack.Formatter

	channels       *ChannelsSidebar
	messages       *MessagesView
	composer       *Composer
	switcher       *QuickSwitcher
	linkPicker     *LinkPicker
	imageViewer    *ImageViewer
	messageEditor  *MessageEditor
	reactionPicker *ReactionPicker
	settings       *SettingsScreen

	mu          sync.Mutex
	channelList []slack.Channel

	activeID       atomic.Value // string
	viewingContext bool

	// keyTag is the focus target for app-level shortcuts (j/k navigation).
	// Bare struct{} is fine — we only use its address.
	keyTag           struct{}
	composerPasteTag struct{}

	// UI mode flags. Mutated from goroutines, read on the UI thread; the
	// reads happen during Layout while writes trigger Invalidate, so a frame
	// boundary acts as the sync point.
	switcherOpen       bool
	linkPickerOpen     bool
	imageViewerOpen    bool
	messageEditorOpen  bool
	reactionPickerOpen bool
	settingsOpen       bool

	// Channel + timestamp captured when the reaction picker opens, so the
	// emoji selection still targets the right message even if the chat view
	// has shifted by the time the user picks.
	reactionTargetCh string
	reactionTargetTS string

	// Which pane j/k currently navigates. Toggled by 'h' (channels) and
	// 'l' (messages). Defaults to channels so existing muscle memory works.
	focusPane focusPane

	// Pending focus commands. Set from any goroutine; consumed on the next
	// frame because key.FocusCmd must run inside Layout via gtx.Execute.
	initFocused             bool
	pendFocusKeyTag         bool
	pendFocusSwitcher       bool
	pendFocusReactionPicker bool
	pendFocusMessageEditor  bool

	// composerVisible toggles the bottom input row. The composer auto-hides
	// when it loses focus and reappears when the user presses 'i'.
	// composerWasFocused tracks whether focus has landed on the editor at
	// least once during the current visibility window so we don't auto-hide
	// before focus has had a chance to take effect.
	composerVisible    bool
	composerWasFocused bool

	backgroundTasks map[string]string // ID -> description
	uploadProgress  atomic.Int32      // 0-100, -1 means no active upload

	// Tracks if the last Up/Down press hit a boundary without moving.
	atUpBoundary   bool
	atDownBoundary bool
}

// Run blocks running the GUI until the window is closed.
func Run(client *slack.Client, cfg *config.Config) error {
	w := &app.Window{}
	w.Option(
		app.Title("wlslack"),
		app.Size(unit.Dp(1100), unit.Dp(700)),
		app.MinSize(unit.Dp(640), unit.Dp(400)),
	)

	a := &App{
		w:      w,
		th:     newTheme(),
		client: client,
		cfg:    cfg,
		fmt:    slack.NewFormatter(client.Cache(), cfg.Display.TimestampFormat),
	}
	a.activeID.Store("")
	images := slack.NewImageLoader(cfg.Token, cfg.Cookie, w.Invalidate)
	a.channels = newChannelsSidebar(a.onChannelSelect)
	state, stateLoaded := config.LoadUIState()
	if !stateLoaded {
		state.DisableLinkUnfurl = cfg.Display.DisableLinkUnfurl
		state.DisableMediaUnfurl = cfg.Display.DisableMediaUnfurl
	}
	a.th.ApplyFontPrefs(prefsFromState(state.Fonts))
	a.th.ApplyThemePrefs(state.ThemeSidebar, state.ThemeMain)
	a.th.ShowOnlyRecentChannels = state.ShowOnlyRecentChannels
	a.th.HideEmptyChannels = state.HideEmptyChannels
	a.th.ShowStatusBar = state.ShowStatusBar
	a.th.DisableLinkUnfurl = state.DisableLinkUnfurl
	a.th.DisableMediaUnfurl = state.DisableMediaUnfurl
	a.client.SetUnfurlSettings(a.th.DisableLinkUnfurl, a.th.DisableMediaUnfurl)
	a.backgroundTasks = make(map[string]string)
	a.channels.SetFavorites(state.Favorites, a.onFavoritesChanged)
	a.channels.SetCollapsedGroups(state.CollapsedGroups, a.onCollapsedGroupsChanged)
	a.messages = newMessagesView(images)
	a.composer = newComposer()
	a.uploadProgress.Store(-1)
	a.switcher = newQuickSwitcher(a.onSwitcherSelect, a.onSwitcherSearch)
	a.linkPicker = newLinkPicker(a.onLinkPickerSelect)
	a.imageViewer = newImageViewer(images)
	a.messageEditor = newMessageEditor()
	a.reactionPicker = newReactionPicker(images, a.onReactionPickerSelect)
	a.reactionPicker.SetEmojis(a.fmt.EmojiCatalog())
	a.settings = newSettingsScreen(a.th, a.onFontsChanged, a.closeSettings)

	go a.pollChannels()
	go a.pollActiveChannel()
	go a.pollEmojis()
	go a.pollPresence()

	return a.loop()
}

func (a *App) pollPresence() {
	t := time.NewTicker(a.cfg.Polling.Presence)
	defer t.Stop()
	for range t.C {
		// Identify users to refresh.
		userIDs := make(map[string]bool)

		// 1. Users in DMs in the sidebar
		for _, ch := range a.client.Cache().GetAllChannels() {
			if ch.IsIM && ch.UserID != "" {
				userIDs[ch.UserID] = true
			}
		}

		// 2. Users in the current message view
		msgs := a.messages.CurrentMessages()
		for _, m := range msgs {
			if m.UserID != "" && !m.IsBot {
				userIDs[m.UserID] = true
			}
		}

		if len(userIDs) == 0 {
			continue
		}

		// Refresh them in the background with a slight stagger to avoid burst limits
		go func(ids []string) {
			sem := make(chan struct{}, 5)
			for _, id := range ids {
				sem <- struct{}{}
				go func(userID string) {
					defer func() { <-sem }()
					_, err := a.client.RefreshUser(userID)
					if err != nil {
						slog.Debug("RefreshUser failed", "user", userID, "error", err)
					}
				}(id)
				time.Sleep(100 * time.Millisecond)
			}
			// Wait for all in this batch to finish before invalidating
			for i := 0; i < 5; i++ {
				sem <- struct{}{}
			}
			a.w.Invalidate()
		}(mapKeys(userIDs))
	}
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (a *App) pollEmojis() {
	a.startTask("emojis", "Fetching emojis")
	defer a.endTask("emojis")
	emojis, err := a.client.GetEmoji()
	if err != nil {
		slog.Error("GetEmoji failed", "error", err)
		return
	}
	a.fmt.SetCustomEmojis(emojis)
	a.w.Invalidate()
	// Update reaction picker with new emojis
	a.mu.Lock()
	a.reactionPicker.SetEmojis(a.fmt.EmojiCatalog())
	a.mu.Unlock()
}

func (a *App) loop() error {
	var ops op.Ops
	for {
		ev := a.w.Event()
		switch e := ev.(type) {
		case app.DestroyEvent:
			return e.Err
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			a.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (a *App) startTask(id, desc string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.backgroundTasks[id] = desc
	a.w.Invalidate()
}

func (a *App) endTask(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.backgroundTasks, id)
	a.w.Invalidate()
}

func (a *App) layoutStatusBar(gtx layout.Context) layout.Dimensions {
	a.mu.Lock()
	tasks := make([]string, 0, len(a.backgroundTasks))
	for _, desc := range a.backgroundTasks {
		tasks = append(tasks, desc)
	}
	a.mu.Unlock()

	sort.Strings(tasks)
	msg := ""
	if len(tasks) > 0 {
		msg = "Working: " + strings.Join(tasks, ", ")
	} else {
		msg = "Ready"
	}

	return withBorder(gtx, a.th.Pal.Border, borders{Top: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, a.th.Pal.BgSidebar, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					p := a.uploadProgress.Load()
					if p < 0 {
						return layout.Dimensions{}
					}
					bar := material.ProgressBar(a.th.Mat, float32(p)/100)
					bar.Color = a.th.Pal.Accent
					bar.TrackColor = WithAlpha(a.th.Pal.Accent, 0x33)
					gtx.Constraints.Min.Y = gtx.Dp(unit.Dp(2))
					return bar.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{
						Top:    unit.Dp(2),
						Bottom: unit.Dp(2),
						Left:   unit.Dp(10),
						Right:  unit.Dp(10),
					}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Caption(a.th.Mat, msg)
						a.th.applyFont(&lbl, a.th.Fonts.StatusBar)
						lbl.Color = a.th.Pal.TextDim
						if len(tasks) > 0 {
							lbl.Color = a.th.Pal.Accent
						}
						return lbl.Layout(gtx)
					})
				}),
			)
		})
	})
}

func (a *App) layout(gtx layout.Context) layout.Dimensions {
	a.applyPendingFocus(gtx)
	a.handleKeys(gtx)
	a.handleClipboardEvents(gtx)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(240))
					gtx.Constraints.Max.X = gtx.Dp(unit.Dp(240))
					return a.channels.Layout(gtx, a.th, a.fmt)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					if a.settingsOpen {
						return a.settings.Layout(gtx)
					}
					if a.switcherOpen {
						return a.switcher.Layout(gtx, a.th)
					}
					if a.linkPickerOpen {
						return a.linkPicker.Layout(gtx, a.th)
					}
					if a.imageViewerOpen {
						return a.imageViewer.Layout(gtx, a.th)
					}
					if a.messageEditorOpen {
						return a.messageEditor.Layout(gtx, a.th)
					}
					if a.reactionPickerOpen {
						return a.reactionPicker.Layout(gtx, a.th)
					}

					// Auto-hide the composer when blurred. composerVisible is flipped
					// on 'i' (to bring it back) and cleared once focus has actually
					// left the editor — checked via gtx.Source.Focused below.
					if a.composerVisible && !gtx.Source.Focused(&a.composer.editor) && a.initFocused {
						// Defer the hide by one frame after focus is dispatched: on
						// the very first 'i' press, composerVisible is set during the
						// same frame the FocusCmd is queued, so Focused() still
						// reports false here. We only hide once the editor has been
						// focused at least once and then lost focus.
						if a.composerWasFocused {
							a.composerVisible = false
							a.composerWasFocused = false
						}
					}
					if gtx.Source.Focused(&a.composer.editor) {
						a.composerWasFocused = true
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							return a.messages.Layout(gtx, a.th, a.fmt)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if !a.composerVisible {
								return layout.Dimensions{}
							}
							placeholder := "Message"
							if id := a.getActiveID(); id != "" {
								if ch := a.client.Cache().GetChannel(id); ch != nil {
									placeholder = "Message #" + ch.Name
								}
							}
							gtx.Constraints.Min.X = gtx.Constraints.Max.X
							return a.composer.Layout(gtx, a.th, a.fmt, placeholder, a.onSend, a.onAttach)
						}),
					)
				}),
			)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !a.th.ShowStatusBar {
				return layout.Dimensions{}
			}
			return a.layoutStatusBar(gtx)
		}),
	)
}

// applyPendingFocus runs queued FocusCmd's. They have to be executed inside
// Layout because gtx.Execute isn't available off-thread.
func (a *App) applyPendingFocus(gtx layout.Context) {
	if !a.initFocused {
		gtx.Execute(key.FocusCmd{Tag: &a.keyTag})
		a.initFocused = true
	}
	if a.pendFocusKeyTag {
		gtx.Execute(key.FocusCmd{Tag: &a.keyTag})
		a.pendFocusKeyTag = false
	}
	if a.pendFocusSwitcher {
		gtx.Execute(key.FocusCmd{Tag: a.switcher.Editor()})
		a.pendFocusSwitcher = false
	}
	if a.pendFocusReactionPicker {
		gtx.Execute(key.FocusCmd{Tag: a.reactionPicker.Editor()})
		a.pendFocusReactionPicker = false
	}
	if a.pendFocusMessageEditor {
		a.messageEditor.Focus(gtx)
		a.pendFocusMessageEditor = false
	}
}

// handleKeys registers app-level shortcuts: Ctrl+K to open the switcher,
// Esc to close it / leave the composer, and j/k to step through channels
// when no editor has focus.
func (a *App) handleKeys(gtx layout.Context) {
	event.Op(gtx.Ops, &a.keyTag)

	composerFocused := gtx.Source.Focused(&a.composer.editor)
	switcherEditor := a.switcher.Editor()
	switcherFocused := gtx.Source.Focused(switcherEditor)
	reactionEditor := a.reactionPicker.Editor()
	reactionFocused := gtx.Source.Focused(reactionEditor)
	messageEditorFocused := gtx.Source.Focused(a.messageEditor.FocusTag())

	filters := []event.Filter{
		// Global jump-to shortcut.
		key.Filter{Name: "K", Required: key.ModCtrl},
		// Global force-refresh shortcut.
		key.Filter{Name: "R", Required: key.ModCtrl},
	}
	if a.messageEditorOpen {
		meTag := a.messageEditor.FocusTag()
		filters = append(filters,
			key.Filter{Focus: meTag, Name: key.NameEscape},
			key.Filter{Focus: meTag, Name: "[", Required: key.ModCtrl},
		)
		filters = append(filters, a.messageEditor.KeyFilters()...)
	}
	if a.switcherOpen {
		filters = append(filters,
			key.Filter{Focus: switcherEditor, Name: key.NameEscape},
			key.Filter{Focus: switcherEditor, Name: "[", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: key.NameUpArrow},
			key.Filter{Focus: switcherEditor, Name: key.NameDownArrow},
			key.Filter{Focus: switcherEditor, Name: key.NameLeftArrow, Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: key.NameRightArrow, Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: key.NameReturn},
			key.Filter{Focus: switcherEditor, Name: key.NameTab},
			key.Filter{Focus: switcherEditor, Name: "N", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "P", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "Y", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "W", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: key.NameDeleteBackward, Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "A", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "E", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "F", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "B", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "C", Required: key.ModCtrl},
		)
	}
	if a.reactionPickerOpen {
		filters = append(filters,
			key.Filter{Focus: reactionEditor, Name: key.NameEscape},
			key.Filter{Focus: reactionEditor, Name: "[", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: key.NameUpArrow},
			key.Filter{Focus: reactionEditor, Name: key.NameDownArrow},
			key.Filter{Focus: reactionEditor, Name: key.NameLeftArrow, Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: key.NameRightArrow, Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: key.NameReturn},
			key.Filter{Focus: reactionEditor, Name: "N", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "P", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "Y", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "W", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: key.NameDeleteBackward, Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "A", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "E", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "F", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "B", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "C", Required: key.ModCtrl},
		)
	}
	if composerFocused {
		filters = append(filters,
			key.Filter{Focus: &a.composer.editor, Name: key.NameEscape},
			key.Filter{Focus: &a.composer.editor, Name: "[", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "W", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: key.NameDeleteBackward, Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "T", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "A", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "E", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "F", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "B", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "C", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "V", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "v", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "P", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: "N", Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: key.NameLeftArrow, Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: key.NameRightArrow, Required: key.ModCtrl},
			key.Filter{Focus: &a.composer.editor, Name: key.NameUpArrow},
			key.Filter{Focus: &a.composer.editor, Name: key.NameDownArrow},
		)
		if a.composer.mentionPicker.Active() {
			filters = append(filters,
				key.Filter{Focus: &a.composer.editor, Name: key.NameUpArrow},
				key.Filter{Focus: &a.composer.editor, Name: key.NameDownArrow},
				key.Filter{Focus: &a.composer.editor, Name: key.NameReturn},
				key.Filter{Focus: &a.composer.editor, Name: key.NameTab},
				key.Filter{Focus: &a.composer.editor, Name: "Y", Required: key.ModCtrl},
			)
		}
	}
	if !composerFocused && !switcherFocused && !reactionFocused && !messageEditorFocused && !a.messageEditorOpen {
		// No Focus on these filters: any non-text-editing focus state (e.g. a
		// channel-row Clickable that grabbed focus on click) still routes
		// j/k/h/l/i here. Without this, clicking a row would silently disable
		// keyboard navigation.
		filters = append(filters,
			key.Filter{Name: "U", Required: key.ModCtrl},
			key.Filter{Name: "J"},
			key.Filter{Name: "K"},
			key.Filter{Name: "I"},
			key.Filter{Name: "H"},
			key.Filter{Name: "L"},
			key.Filter{Name: "E"},
			key.Filter{Name: "D", Optional: key.ModShift},
			key.Filter{Name: "Y"},
			key.Filter{Name: "R", Optional: key.ModShift},
			key.Filter{Name: "Q"},
			key.Filter{Name: "F"},
			key.Filter{Name: "F", Required: key.ModCtrl},
			key.Filter{Name: "B", Required: key.ModCtrl},
			key.Filter{Name: ","},
			key.Filter{Name: key.NameReturn},
			key.Filter{Name: key.NameSpace},
		)
	}
	if a.linkPickerOpen || a.imageViewerOpen {
		filters = append(filters,
			key.Filter{Name: key.NameEscape},
			key.Filter{Name: "[", Required: key.ModCtrl},
			key.Filter{Name: "H"},
			key.Filter{Name: "Q"},
			key.Filter{Name: key.NameUpArrow},
			key.Filter{Name: key.NameDownArrow},
			key.Filter{Name: key.NameLeftArrow},
			key.Filter{Name: key.NameRightArrow},
		)
	}
	if a.settingsOpen {
		filters = append(filters,
			key.Filter{Name: key.NameEscape},
			key.Filter{Name: "[", Required: key.ModCtrl},
		)
	}

	for {
		ev, ok := gtx.Source.Event(filters...)
		if !ok {
			break
		}
		kev, ok := ev.(key.Event)
		if !ok || kev.State != key.Press {
			continue
		}
		switch {
		case kev.Name == "K" && kev.Modifiers.Contain(key.ModCtrl):
			if a.switcherOpen {
				a.closeSwitcher()
			} else {
				a.openSwitcher()
			}
		case kev.Name == "R" && kev.Modifiers.Contain(key.ModCtrl):
			a.forceRefresh()
		case kev.Name == key.NameEscape || (kev.Name == "[" && kev.Modifiers.Contain(key.ModCtrl)):
			switch {
			case a.settingsOpen:
				a.closeSettings()
			case a.switcherOpen:
				a.closeSwitcher()
			case a.reactionPickerOpen:
				a.closeReactionPicker()
			case a.linkPickerOpen:
				a.closeLinkPicker()
			case a.imageViewerOpen:
				a.closeImageViewer()
			case a.messageEditorOpen:
				if !a.messageEditor.HandleEsc() {
					a.closeMessageEditor()
				}
			case composerFocused:
				a.pendFocusKeyTag = true
				a.w.Invalidate()
			case a.messages.AuthorOpen():
				a.messages.CloseAuthor()
				a.w.Invalidate()
			}
		case a.messageEditorOpen:
			if a.messageEditor.HandleKey(gtx, kev) {
				a.w.Invalidate()
			}
		case a.imageViewerOpen && (kev.Name == key.NameLeftArrow || kev.Name == key.NameUpArrow):
			a.imageViewer.MoveSelection(-1)
			a.w.Invalidate()
		case a.imageViewerOpen && (kev.Name == key.NameRightArrow || kev.Name == key.NameDownArrow):
			a.imageViewer.MoveSelection(1)
			a.w.Invalidate()
		case a.linkPickerOpen && (kev.Name == key.NameUpArrow):
			a.linkPicker.MoveSelection(-1)
			a.w.Invalidate()
		case a.linkPickerOpen && (kev.Name == key.NameDownArrow):
			a.linkPicker.MoveSelection(1)
			a.w.Invalidate()
		case a.linkPickerOpen && kev.Name == key.NameReturn:
			a.linkPicker.Submit()
		case (kev.Name == key.NameReturn || kev.Name == key.NameSpace) && a.focusPane == paneChannels && !a.switcherOpen && !a.reactionPickerOpen && !a.linkPickerOpen && !a.imageViewerOpen:
			if a.channels.ToggleCursorHeader() {
				a.w.Invalidate()
			}
		case kev.Name == key.NameReturn && a.focusPane == paneMessages && !a.switcherOpen && !a.reactionPickerOpen:
			if ts := a.messages.DeletePendingTS(); ts != "" {
				msg, msgTS, ok := a.messages.SelectedMessage()
				if ok && msgTS == ts {
					ch := a.getActiveID()
					if ch == "__UNREADS__" && msg.ChannelID != "" {
						ch = msg.ChannelID
					}
					if a.messages.InThread() {
						ch, _ = a.messages.ThreadInfo()
					}
					a.deleteMessage(ch, ts)
					a.messages.SetDeletePendingTS("")
					a.w.Invalidate()
					continue
				}
			}
			a.openSelectedLinks()
		case kev.Name == "D":
			if kev.Modifiers.Contain(key.ModShift) {
				if a.focusPane != paneMessages {
					a.startTask("download_err", "Focus the messages pane (l) to download attachments")
					go func() { time.Sleep(3 * time.Second); a.endTask("download_err") }()
					break
				}
				msg, _, ok := a.messages.SelectedMessage()
				if !ok {
					a.startTask("download_err", "Select a message to download attachments")
					go func() { time.Sleep(3 * time.Second); a.endTask("download_err") }()
					break
				}
				if len(msg.Files) == 0 {
					a.startTask("download_err", "Selected message has no attachments")
					go func() { time.Sleep(3 * time.Second); a.endTask("download_err") }()
					break
				}
				go a.downloadAttachments(msg)
				break
			}
			if a.focusPane == paneMessages {
				msg, ts, ok := a.messages.SelectedMessage()
				if ok && msg.UserID == a.client.GetSelfID() {
					if a.messages.DeletePendingTS() == ts {
						ch := a.getActiveID()
						if ch == "__UNREADS__" && msg.ChannelID != "" {
							ch = msg.ChannelID
						}
						if a.messages.InThread() {
							ch, _ = a.messages.ThreadInfo()
						}
						a.deleteMessage(ch, ts)
						a.messages.SetDeletePendingTS("")
					} else {
						a.messages.SetDeletePendingTS(ts)
					}
					a.w.Invalidate()
				}
			}
		case a.switcherOpen && (kev.Name == key.NameUpArrow || (kev.Name == "P" && kev.Modifiers.Contain(key.ModCtrl))):
			a.switcher.MoveSelection(-1)
			a.w.Invalidate()
		case a.switcherOpen && (kev.Name == key.NameDownArrow || (kev.Name == "N" && kev.Modifiers.Contain(key.ModCtrl))):
			a.switcher.MoveSelection(1)
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == key.NameLeftArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.MoveWord(-1)
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == key.NameRightArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.MoveWord(1)
			a.w.Invalidate()
		case a.switcherOpen && (kev.Name == "W" || kev.Name == key.NameDeleteBackward) && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.DeleteLastWord()
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == "A" && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.SelectAll()
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == "E" && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.MoveToEnd()
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == "F" && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.MoveCursor(1)
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == "B" && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.MoveCursor(-1)
			a.w.Invalidate()
		case a.switcherOpen && kev.Name == "C" && kev.Modifiers.Contain(key.ModCtrl):
			a.switcher.Clear()
			a.w.Invalidate()
		case a.switcherOpen && (kev.Name == key.NameReturn || (kev.Name == "Y" && kev.Modifiers.Contain(key.ModCtrl))):
			a.switcher.Submit()
		case a.switcherOpen && kev.Name == key.NameTab:
			a.switcher.ToggleTab()
			a.w.Invalidate()
		case a.reactionPickerOpen && (kev.Name == key.NameUpArrow || (kev.Name == "P" && kev.Modifiers.Contain(key.ModCtrl))):
			a.reactionPicker.MoveSelection(-1)
			a.w.Invalidate()
		case a.reactionPickerOpen && (kev.Name == key.NameDownArrow || (kev.Name == "N" && kev.Modifiers.Contain(key.ModCtrl))):
			a.reactionPicker.MoveSelection(1)
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == key.NameLeftArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.MoveWord(-1)
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == key.NameRightArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.MoveWord(1)
			a.w.Invalidate()
		case a.reactionPickerOpen && (kev.Name == "W" || kev.Name == key.NameDeleteBackward) && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.DeleteLastWord()
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == "A" && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.SelectAll()
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == "E" && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.MoveToEnd()
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == "F" && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.MoveCursor(1)
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == "B" && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.MoveCursor(-1)
			a.w.Invalidate()
		case composerFocused && kev.Name == "C" && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.Clear()
			a.w.Invalidate()
		case composerFocused && (kev.Name == "V" || kev.Name == "v") && kev.Modifiers.Contain(key.ModCtrl):
			slog.Info("Ctrl+V detected", "tag", fmt.Sprintf("%p", &a.composerPasteTag))
			if !a.tryWaylandPaste() {
				gtx.Execute(clipboard.ReadCmd{Tag: &a.composerPasteTag})
			}
		case composerFocused && (kev.Name == "W" || kev.Name == key.NameDeleteBackward) && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.DeleteLastWord()
			a.w.Invalidate()
		case composerFocused && kev.Name == key.NameLeftArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.MoveWord(-1)
			a.w.Invalidate()
		case composerFocused && kev.Name == key.NameRightArrow && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.MoveWord(1)
			a.w.Invalidate()
		case composerFocused && kev.Name == "A" && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.SelectAll()
			a.w.Invalidate()
		case composerFocused && kev.Name == "E" && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.MoveToEnd()
			a.w.Invalidate()
		case composerFocused && kev.Name == "T" && kev.Modifiers.Contain(key.ModCtrl):
			a.composer.TranslateToEnglish(func(text string, setFeedback func(string), done func(string, error)) {
				urls := llm.ExtractURLs(text)
				if len(urls) == 1 {
					setFeedback("Summarizing link...")
					go func() {
						a.startTask("llm", "Summarizing link")
						defer a.endTask("llm")
						summarized, err := llm.SummarizeLink(context.Background(), text, urls[0])
						if err != nil {
							slog.Error("llm summarize failed", "error", err)
							done("", err)
							a.w.Invalidate()
							return
						}
						done(summarized, nil)
						a.w.Invalidate()
					}()
					return
				}

				setFeedback("Translating...")
				go func() {
					a.startTask("translate", "Translating")
					defer a.endTask("translate")
					translated, err := translate.ToEnglish(context.Background(), text)
					if err != nil {
						slog.Error("translate failed", "error", err)
						done("", err)
						a.w.Invalidate()
						return
					}
					done(translated, nil)
					a.w.Invalidate()
				}()
			})
			a.w.Invalidate()
		case composerFocused && a.composer.mentionPicker.Active() && kev.Name == key.NameUpArrow:
			a.composer.mentionPicker.MoveSelection(-1)
			a.atUpBoundary = false
			a.atDownBoundary = false
			a.w.Invalidate()
		case composerFocused && kev.Name == key.NameUpArrow:
			oldPos, _ := a.composer.editor.Selection()
			a.composer.MoveLine(-1)
			newPos, _ := a.composer.editor.Selection()
			if oldPos == newPos {
				if a.atUpBoundary {
					if oldPos == 0 {
						a.composer.HistoryPrev()
					} else {
						a.composer.MoveToStart()
					}
				} else {
					a.atUpBoundary = true
				}
			} else {
				a.atUpBoundary = false
			}
			a.atDownBoundary = false
			a.w.Invalidate()
		case composerFocused && a.composer.mentionPicker.Active() && kev.Name == key.NameDownArrow:
			a.composer.mentionPicker.MoveSelection(1)
			a.atUpBoundary = false
			a.atDownBoundary = false
			a.w.Invalidate()
		case composerFocused && kev.Name == key.NameDownArrow:
			oldPos, _ := a.composer.editor.Selection()
			a.composer.MoveLine(1)
			newPos, _ := a.composer.editor.Selection()
			if oldPos == newPos {
				lastPos := len([]rune(a.composer.editor.Text()))
				if a.atDownBoundary {
					if oldPos == lastPos {
						a.composer.HistoryNext()
					} else {
						a.composer.MoveToEnd()
					}
				} else {
					a.atDownBoundary = true
				}
			} else {
				a.atDownBoundary = false
			}
			a.atUpBoundary = false
			a.w.Invalidate()
		case composerFocused && kev.Name == "P" && kev.Modifiers.Contain(key.ModCtrl):
			if a.composer.mentionPicker.Active() {
				a.composer.mentionPicker.MoveSelection(-1)
			} else {
				a.composer.HistoryPrev()
			}
			a.w.Invalidate()
		case composerFocused && kev.Name == "N" && kev.Modifiers.Contain(key.ModCtrl):
			if a.composer.mentionPicker.Active() {
				a.composer.mentionPicker.MoveSelection(1)
			} else {
				a.composer.HistoryNext()
			}
			a.w.Invalidate()
		case composerFocused && a.composer.mentionPicker.Active() && (kev.Name == key.NameReturn || kev.Name == key.NameTab || (kev.Name == "Y" && kev.Modifiers.Contain(key.ModCtrl))):
			a.composer.mentionPicker.Submit()
			a.w.Invalidate()
		case a.reactionPickerOpen && (kev.Name == key.NameReturn || (kev.Name == "Y" && kev.Modifiers.Contain(key.ModCtrl))):
			a.reactionPicker.Submit()
		case kev.Name == "U" && kev.Modifiers.Contain(key.ModCtrl):
			if a.focusPane == paneMessages {
				a.toggleSelectedPreview()
			}
		case kev.Name == "J":
			switch {
			case a.linkPickerOpen:
				a.linkPicker.MoveSelection(1)
				a.w.Invalidate()
			case a.imageViewerOpen:
				a.imageViewer.MoveSelection(1)
				a.w.Invalidate()
			default:
				a.moveInPane(1)
			}
		case kev.Name == "K":
			switch {
			case a.linkPickerOpen:
				a.linkPicker.MoveSelection(-1)
				a.w.Invalidate()
			case a.imageViewerOpen:
				a.imageViewer.MoveSelection(-1)
				a.w.Invalidate()
			default:
				a.moveInPane(-1)
			}
		case kev.Name == "F" && kev.Modifiers.Contain(key.ModCtrl):
			a.pageInPane(1)
		case kev.Name == "F":
			if a.focusPane == paneChannels {
				if a.channels.ToggleFavoriteOnActive() {
					a.w.Invalidate()
				}
			}
		case kev.Name == "B" && kev.Modifiers.Contain(key.ModCtrl):
			a.pageInPane(-1)
		case kev.Name == "H":
			// h peels back one layer at a time: link picker → author panel →
			// thread → channels-pane focus. This keeps h/l symmetrical with the
			// l-to-drill-in progression.
			switch {
			case a.linkPickerOpen:
				a.closeLinkPicker()
			case a.imageViewerOpen:
				a.closeImageViewer()
			case a.messages.AuthorOpen():
				a.messages.CloseAuthor()
				a.w.Invalidate()
			case a.messages.CloseThread():
				a.w.Invalidate()
			default:
				a.setFocusPane(paneChannels)
			}

		case kev.Name == "L":
			// Drill-in progression on the messages pane:
			//   channel-history selection → thread view
			//   thread selection         → author detail panel
			// Without a selection, l just moves focus to the messages pane.
			switch {
			case a.focusPane == paneMessages && a.messages.HasThreadSelection():
				if a.messages.OpenAuthor(a.fmt) {
					a.w.Invalidate()
				}
			case a.focusPane == paneMessages && a.messages.HasSelection():
				a.openThread()
			default:
				a.setFocusPane(paneMessages)
			}
		case kev.Name == "E":
			if a.focusPane == paneMessages {
				a.openMessageEditor()
			}
		case kev.Name == "Y":
			if a.messages.AuthorOpen() {
				if v, ok := a.messages.AuthorSelectedValue(); ok && v != "" {
					gtx.Execute(clipboard.WriteCmd{
						Type: "application/text",
						Data: io.NopCloser(strings.NewReader(v)),
					})
				}
			} else if a.focusPane == paneMessages {
				msg, _, ok := a.messages.SelectedMessage()
				if ok {
					text := a.fmt.Format(msg.Text)
					for _, f := range msg.Files {
						if text != "" {
							text += "\n"
						}
						text += f.PreferredImageURL()
					}
					if text != "" {
						gtx.Execute(clipboard.WriteCmd{
							Type: "application/text",
							Data: io.NopCloser(strings.NewReader(text)),
						})
					}
				}
			}
		case kev.Name == "R":
			// Shift differentiates the two reaction shortcuts: capital R
			// echoes every reaction already on the message, lowercase r
			// opens the picker so the user can choose a new one.
			if kev.Modifiers.Contain(key.ModShift) {
				a.echoReactions()
			} else {
				a.openReactionPicker()
			}
		case kev.Name == "I":
			a.composerVisible = true
			a.composerWasFocused = false
			gtx.Execute(key.FocusCmd{Tag: &a.composer.editor})
			a.w.Invalidate()
		case kev.Name == ",":
			if a.settingsOpen {
				a.closeSettings()
			} else {
				a.openSettings()
			}
		case kev.Name == "Q":
			switch {
			case a.linkPickerOpen:
				a.closeLinkPicker()
			case a.imageViewerOpen:
				a.closeImageViewer()
			default:
				os.Exit(0)
			}
		}
	}
}

func (a *App) openSwitcher() {
	a.switcher.Reset()
	a.switcher.SetChannels(a.client.Cache().GetAllChannels())
	a.switcherOpen = true
	a.pendFocusSwitcher = true
	a.w.Invalidate()
}

func (a *App) closeSwitcher() {
	a.switcherOpen = false
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

func (a *App) onSwitcherSelect(id string, ts string) {
	a.onChannelSelectWithContext(id, ts)
	if ts != "" {
		a.messages.FocusMessage(ts)
	} else {
		a.messages.FocusLast()
	}
	a.setFocusPane(paneMessages)
	a.closeSwitcher()
}

func (a *App) onSwitcherSearch(query string) {
	go func() {
		results, err := a.client.Search(query)
		if err != nil {
			slog.Error("search failed", "error", err)
			return
		}
		a.switcher.SetResults(results)
		a.w.Invalidate()
	}()
}

// openSelectedLinks runs Enter on the highlighted message. Resolution order:
//   - 1 link → open in browser
//   - >1 links → link picker
//   - 0 links and >1 images → in-app image viewer
//   - everything else → no-op (single inline image already shows in the row)
func (a *App) openSelectedLinks() {
	urls := a.messages.SelectedMessageURLs()
	switch len(urls) {
	case 0:
		images := a.messages.SelectedMessageImages()
		if len(images) >= 1 {
			a.imageViewer.SetFiles(images)
			a.imageViewerOpen = true
			a.w.Invalidate()
		}
	case 1:
		openURL(urls[0])
	default:
		a.linkPicker.SetURLs(urls)
		a.linkPickerOpen = true
		a.w.Invalidate()
	}
}

func (a *App) closeImageViewer() {
	a.imageViewerOpen = false
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

func (a *App) openMessageEditor() {
	msg, _, ok := a.messages.SelectedMessage()
	if !ok {
		return
	}
	text := a.fmt.Format(msg.Text)
	for _, f := range msg.Files {
		if text != "" {
			text += "\n"
		}
		text += f.PreferredImageURL()
	}
	if text == "" {
		return
	}
	a.messageEditor.SetText(text)
	a.messageEditorOpen = true
	a.pendFocusMessageEditor = true
	a.w.Invalidate()
}

func (a *App) closeMessageEditor() {
	a.messageEditorOpen = false
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

func (a *App) closeLinkPicker() {
	a.linkPickerOpen = false
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

func (a *App) onLinkPickerSelect(url string) {
	openURL(url)
	a.closeLinkPicker()
}

// openReactionPicker drops the picker over the message pane, anchored to the
// currently highlighted message. The channel + ts are captured up front so a
// later refresh that shifts row indices doesn't redirect the reaction.
func (a *App) openReactionPicker() {
	msg, ts, ok := a.messages.SelectedMessage()
	if !ok {
		return
	}
	chID := a.getActiveID()
	if chID == "__UNREADS__" && msg.ChannelID != "" {
		chID = msg.ChannelID
	}
	if chID == "" {
		return
	}

	a.reactionTargetCh = chID
	a.reactionTargetTS = ts
	a.reactionPicker.Reset(msg.Reactions, a.fmt)
	a.reactionPickerOpen = true
	a.pendFocusReactionPicker = true
	a.w.Invalidate()
}

func (a *App) closeReactionPicker() {
	a.reactionPickerOpen = false
	a.reactionTargetCh = ""
	a.reactionTargetTS = ""
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

func (a *App) onReactionPickerSelect(name string) {
	chID, ts := a.reactionTargetCh, a.reactionTargetTS
	a.closeReactionPicker()
	if chID == "" || ts == "" || name == "" {
		return
	}
	go func() {
		if err := a.client.AddReaction(chID, ts, name); err != nil {
			slog.Error("AddReaction failed", "channel", chID, "ts", ts, "emoji", name, "error", err)
			return
		}
		a.refreshAfterReaction(chID, ts)
	}()
}

// echoReactions adds the current user's reaction to every emoji already on
// the highlighted message, skipping the ones the user has already reacted
// to. Triggered by capital R. If the user has already reacted with every
// emoji present on the message, it instead removes all their reactions.
func (a *App) echoReactions() {
	msg, ts, ok := a.messages.SelectedMessage()
	if !ok {
		return
	}
	chID := a.getActiveID()
	if chID == "__UNREADS__" && msg.ChannelID != "" {
		chID = msg.ChannelID
	}
	if chID == "" {
		return
	}

	allHaveMe := len(msg.Reactions) > 0
	toAdd := make([]string, 0, len(msg.Reactions))
	toRemove := make([]string, 0, len(msg.Reactions))

	for _, r := range msg.Reactions {
		if r.Name == "" {
			continue
		}
		if r.HasMe {
			toRemove = append(toRemove, r.Name)
		} else {
			allHaveMe = false
			toAdd = append(toAdd, r.Name)
		}
	}

	go func() {
		if allHaveMe {
			for _, name := range toRemove {
				if err := a.client.RemoveReaction(chID, ts, name); err != nil {
					slog.Error("RemoveReaction failed", "channel", chID, "ts", ts, "emoji", name, "error", err)
				}
			}
		} else {
			if len(toAdd) == 0 {
				return
			}
			for _, name := range toAdd {
				if err := a.client.AddReaction(chID, ts, name); err != nil {
					slog.Error("AddReaction failed", "channel", chID, "ts", ts, "emoji", name, "error", err)
				}
			}
		}
		a.refreshAfterReaction(chID, ts)
	}()
}

// refreshAfterReaction repulls whichever view (channel history or thread)
// the reaction landed in, so the new reaction count surfaces without waiting
// on the next polling tick.
func (a *App) refreshAfterReaction(chID, ts string) {
	if a.messages.InThread() {
		curCh, threadTS := a.messages.ThreadInfo()
		if curCh == chID && threadTS != "" {
			a.fetchThread(chID, threadTS)
			a.w.Invalidate()
			return
		}
	}
	if a.getActiveID() == chID {
		a.fetchMessages(chID)
		a.w.Invalidate()
	}
	_ = ts
}

func (a *App) pickDirectory() (string, error) {
	// Try zenity first
	cmd := exec.Command("zenity", "--file-selection", "--directory", "--title=Select Download Directory")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	// Fallback to kdialog
	cmd = exec.Command("kdialog", "--getexistingdirectory")
	out, err = cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return "", fmt.Errorf("no directory picker available (install zenity or kdialog)")
}

func (a *App) downloadAttachments(msg slack.Message) {
	dir, err := a.pickDirectory()
	if err != nil {
		slog.Error("pick directory failed", "error", err)
		a.startTask("download_err", "Download failed: "+err.Error())
		go func() {
			time.Sleep(5 * time.Second)
			a.endTask("download_err")
		}()
		return
	}
	if dir == "" {
		return
	}

	a.startTask("download", fmt.Sprintf("Downloading %d files", len(msg.Files)))
	defer a.endTask("download")

	for _, f := range msg.Files {
		if f.URL == "" {
			continue
		}
		dest := filepath.Join(dir, f.Name)
		// Handle duplicate filenames
		if _, err := os.Stat(dest); err == nil {
			ext := filepath.Ext(f.Name)
			base := strings.TrimSuffix(f.Name, ext)
			dest = filepath.Join(dir, fmt.Sprintf("%s_%s%s", base, time.Now().Format("150405"), ext))
		}

		err := a.client.DownloadFile(f.URL, dest)
		if err != nil {
			slog.Error("download failed", "file", f.Name, "error", err)
		}
	}
}

func (a *App) toggleSelectedPreview() {
	msg, ts, ok := a.messages.SelectedMessage()
	if !ok || msg.UserID != a.client.GetSelfID() {
		return
	}
	chID := a.getActiveID()
	if chID == "__UNREADS__" && msg.ChannelID != "" {
		chID = msg.ChannelID
	}
	if chID == "" {
		return
	}

	go func() {
		// Update the message with its own text but respect current client settings
		if err := a.client.UpdateMessage(chID, ts, msg.Text); err != nil {
			slog.Error("UpdateMessage for preview toggle failed", "channel", chID, "ts", ts, "error", err)
			return
		}
		if a.messages.InThread() {
			tCh, tTS := a.messages.ThreadInfo()
			if tCh == chID {
				a.fetchThread(chID, tTS)
			}
		}
		if a.getActiveID() == chID {
			a.fetchMessages(chID)
		}
		a.w.Invalidate()
	}()
}

func (a *App) deleteMessage(ch, ts string) {
	go func() {
		if err := a.client.DeleteMessage(ch, ts); err != nil {
			slog.Error("DeleteMessage failed", "channel", ch, "ts", ts, "error", err)
			return
		}
		if a.messages.InThread() {
			tCh, tTS := a.messages.ThreadInfo()
			if tCh == ch {
				a.fetchThread(ch, tTS)
			}
		}
		if a.getActiveID() == ch {
			a.fetchMessages(ch)
		}
		a.w.Invalidate()
	}()
}

// openURL hands a URL to the system browser via xdg-open. Errors are logged
// but not surfaced — the user just sees no browser pop, which is consistent
// with how desktop launchers usually behave.
func openURL(url string) {
	if url == "" {
		return
	}
	go func() {
		cmd := exec.Command("xdg-open", url)
		if err := cmd.Start(); err != nil {
			slog.Error("xdg-open failed", "url", url, "error", err)
			return
		}
		// Reap the child so it doesn't linger as a zombie.
		go func() { _ = cmd.Wait() }()
	}()
}

// onFavoritesChanged persists the updated favorites list. The sidebar fires
// this on every toggle; we merge into the existing UIState file so unrelated
// state (read timestamps, etc.) stays intact.
func (a *App) onFavoritesChanged(ids []string) {
	state, _ := config.LoadUIState()
	state.Favorites = ids
	config.SaveUIState(state)
}

func (a *App) onCollapsedGroupsChanged(keys []string) {
	state, _ := config.LoadUIState()
	state.CollapsedGroups = keys
	config.SaveUIState(state)
}

func (a *App) openSettings() {
	a.settingsOpen = true
	a.w.Invalidate()
}

func (a *App) closeSettings() {
	a.settingsOpen = false
	a.pendFocusKeyTag = true
	a.w.Invalidate()
}

// onFontsChanged is fired by the SettingsScreen on every face/size mutation so
// we can redraw and persist immediately. Disk write is cheap; we do it inline
// for simplicity.
func (a *App) onFontsChanged() {
	state, _ := config.LoadUIState()
	state.Fonts = stateFromTheme(a.th)
	state.ThemeSidebar = a.th.ThemeSidebar
	state.ThemeMain = a.th.ThemeMain
	state.ShowOnlyRecentChannels = a.th.ShowOnlyRecentChannels
	state.HideEmptyChannels = a.th.HideEmptyChannels
	state.ShowStatusBar = a.th.ShowStatusBar
	state.DisableLinkUnfurl = a.th.DisableLinkUnfurl
	state.DisableMediaUnfurl = a.th.DisableMediaUnfurl
	config.SaveUIState(state)
	a.client.SetUnfurlSettings(a.th.DisableLinkUnfurl, a.th.DisableMediaUnfurl)
	a.w.Invalidate()
}

// prefsFromState bridges config.FontPrefs → ui sectionPrefs without making
// the ui package import a runtime dependency on the JSON struct shape.
func prefsFromState(p config.FontPrefs) sectionPrefs {
	conv := func(f config.FontPref) FontStyle { return FontStyle{Face: f.Face, Size: f.Size} }
	return sectionPrefs{
		Global:    conv(p.Global),
		Channels:  conv(p.Channels),
		Header:    conv(p.Header),
		Messages:  conv(p.Messages),
		Threads:   conv(p.Threads),
		Composer:  conv(p.Composer),
		Code:      conv(p.Code),
		Search:    conv(p.Search),
		UserInfo:  conv(p.UserInfo),
		StatusBar: conv(p.StatusBar),
	}
}

func stateFromTheme(th *Theme) config.FontPrefs {
	conv := func(f FontStyle) config.FontPref { return config.FontPref{Face: f.Face, Size: f.Size} }
	return config.FontPrefs{
		Global:    conv(th.Fonts.Global),
		Channels:  conv(th.Fonts.Channels),
		Header:    conv(th.Fonts.Header),
		Messages:  conv(th.Fonts.Messages),
		Threads:   conv(th.Fonts.Threads),
		Composer:  conv(th.Fonts.Composer),
		Code:      conv(th.Fonts.Code),
		Search:    conv(th.Fonts.Search),
		UserInfo:  conv(th.Fonts.UserInfo),
		StatusBar: conv(th.Fonts.StatusBar),
	}
}

// moveInPane dispatches j/k to whichever pane has logical focus. The author
// panel takes priority — when it's open, j/k walks the field list rather than
// the underlying message rows.
func (a *App) moveInPane(delta int) {
	if a.messages.AuthorOpen() {
		if a.messages.MoveAuthorSelection(delta) {
			a.w.Invalidate()
		}
		return
	}
	switch a.focusPane {
	case paneMessages:
		if a.messages.MoveSelection(delta) {
			a.w.Invalidate()
		}
	default:
		if id, ok := a.channels.MoveSelection(delta); ok {
			if id != "" {
				a.onChannelSelect(id)
			} else {
				a.w.Invalidate()
			}
		}
	}
}

// pageInPane dispatches page up/down to whichever pane has logical focus.
func (a *App) pageInPane(dir int) {
	switch a.focusPane {
	case paneMessages:
		delta := a.messages.PageSize() * dir
		if a.messages.MoveSelection(delta) {
			a.w.Invalidate()
		}
	default:
		delta := a.channels.PageSize() * dir
		if id, ok := a.channels.MoveSelection(delta); ok {
			if id != "" {
				a.onChannelSelect(id)
			} else {
				a.w.Invalidate()
			}
		}
	}
}

// setFocusPane swaps which pane the keyboard targets and updates the
// MessagesView highlight so the user can see where focus went.
func (a *App) setFocusPane(p focusPane) {
	if a.focusPane == p {
		return
	}
	a.focusPane = p
	a.messages.SetFocused(p == paneMessages)
	a.w.Invalidate()
}

// autoSelectFirst picks the top channel for the user when none is active yet,
// so the right panel shows content immediately on startup instead of waiting
// for a click or j/k keypress.
func (a *App) autoSelectFirst(channels []slack.Channel) {
	if a.getActiveID() != "" || len(channels) == 0 {
		return
	}
	id := a.channels.FirstID()
	if id == "" {
		return
	}
	a.onChannelSelect(id)
}

func (a *App) onChannelSelect(id string) {
	a.onChannelSelectWithContext(id, "")
}

func (a *App) onChannelSelectWithContext(id string, ts string) {
	a.activeID.Store(id)
	a.channels.SetActive(id)
	a.messages.Reset()
	if id == "__UNREADS__" {
		a.messages.SetHeader("All Unreads", "Consolidated view of all unread messages across all channels")
		a.messages.SetMessages(nil)
		a.w.Invalidate()
		go a.fetchAllUnreads()
		return
	}
	if ch := a.client.Cache().GetChannel(id); ch != nil {
		a.messages.SetHeader(ch.Name, ch.Topic)
		if ch.UnreadCount > 0 && ch.LatestTS != "" {
			// Optimistically clear the unread count in cache
			a.client.Cache().SetChannelUnread(ch.ID, 0, 0, ch.LatestTS, ch.LatestTS)
			a.channels.SetChannels(a.client.Cache().GetAllChannels())

			go func(channelID, latestTS string) {
				if err := a.client.MarkChannel(channelID, latestTS); err != nil {
					slog.Debug("MarkChannel failed", "channel", channelID, "error", err)
				}
			}(ch.ID, ch.LatestTS)
		}
	}
	// Show whatever is already cached immediately, then trigger a fresh fetch.
	cached := a.client.Cache().GetMessages(id)
	a.messages.SetMessages(cached)
	a.w.Invalidate()
	if ts != "" {
		a.viewingContext = true
		go a.fetchMessagesAround(id, ts)
	} else {
		a.viewingContext = false
		go a.fetchMessages(id)
	}
}

func (a *App) onSend(text string, attachments []Attachment) {
	id := a.getActiveID()
	if id == "" {
		return
	}
	a.pendFocusKeyTag = true
	a.setFocusPane(paneMessages)

	threadTS := ""
	if a.messages.InThread() {
		_, threadTS = a.messages.ThreadInfo()
	}

	go func() {
		if len(attachments) > 0 {
			a.startTask("upload", fmt.Sprintf("Uploading %d files", len(attachments)))
			a.uploadProgress.Store(0)
			defer func() {
				a.endTask("upload")
				a.uploadProgress.Store(-1)
			}()

			for i, att := range attachments {
				comment := ""
				if i == 0 {
					comment = text
				}
				err := a.client.UploadFile(id, threadTS, att.Name, att.Data, comment, func(p float32) {
					// Overall progress: (i + p) / len(attachments)
					overall := (float32(i) + p) / float32(len(attachments))
					a.uploadProgress.Store(int32(overall * 100))
					a.w.Invalidate()
				})
				if err != nil {
					slog.Error("upload failed", "file", att.Name, "error", err)
					// Continue with next? For now yes.
				}
			}
		} else {
			a.startTask("send", "Sending message")
			defer a.endTask("send")
			var err error
			if threadTS != "" {
				err = a.client.SendThreadReply(id, threadTS, text)
			} else {
				err = a.client.SendMessage(id, text)
			}
			if err != nil {
				slog.Error("send failed", "channel", id, "thread", threadTS, "error", err)
				return
			}
		}

		// Refetch to surface the message.
		if threadTS != "" {
			a.fetchThread(id, threadTS)
		} else {
			a.fetchMessages(id)
		}
		a.w.Invalidate()
	}()
}

// openThread enters thread mode for the highlighted message and kicks off a
// background fetch of replies. The cached thread (if any) renders immediately
// so there's no blank flash while the API call is in flight.
func (a *App) openThread() {
	chID, ts, ok := a.messages.OpenThread(a.getActiveID())
	if !ok {
		return
	}
	if cached := a.client.Cache().GetThread(chID, ts); len(cached) > 0 {
		a.messages.SetThreadMessages(cached)
	}
	a.w.Invalidate()
	go a.fetchThread(chID, ts)
}

func (a *App) fetchThread(channelID, threadTS string) {
	a.startTask("thread:"+channelID+":"+threadTS, "Fetching thread")
	defer a.endTask("thread:" + channelID + ":" + threadTS)
	msgs, err := a.client.GetThreadReplies(channelID, threadTS)
	if err != nil {
		slog.Error("GetThreadReplies failed", "channel", channelID, "ts", threadTS, "error", err)
		return
	}
	// Drop the result if the user has since closed the thread or moved to a
	// different one — otherwise we'd silently overwrite their current view.
	if !a.messages.InThread() {
		return
	}
	curCh, curTS := a.messages.ThreadInfo()
	if curCh != channelID || curTS != threadTS {
		return
	}
	a.messages.SetThreadMessages(msgs)
	a.w.Invalidate()
}

func (a *App) getActiveID() string {
	v := a.activeID.Load()
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// pollChannels refreshes the channel list, then keeps pulling on the
// configured interval. The first refresh happens immediately so the user
// doesn't stare at an empty sidebar.
func (a *App) isMention(msg slack.Message) bool {
	selfID := a.client.GetSelfID()
	groups := a.client.Cache().GetAllUserGroups()

	// User mention: <@U123>
	if strings.Contains(msg.Text, "<@"+selfID+">") {
		return true
	}
	// Group mention: <!subteam^S123|@handle>
	for _, g := range groups {
		if strings.Contains(msg.Text, "<!subteam^"+g.ID) {
			return true
		}
	}
	// Special mentions: <!here>, <!channel>, <!everyone>
	if strings.Contains(msg.Text, "<!here>") ||
		strings.Contains(msg.Text, "<!channel>") ||
		strings.Contains(msg.Text, "<!everyone>") {
		return true
	}
	return false
}

func (a *App) pollChannels() {
	// Try cached data first for instant UI.
	if cached, err := a.client.Cache().LoadChannelsFromDisk(); err == nil && len(cached) > 0 {
		a.channels.SetChannels(cached)
		a.autoSelectFirst(cached)
		a.w.Invalidate()
	}
	_, _ = a.client.Cache().LoadUsersFromDisk()
	_, _ = a.client.Cache().LoadUserGroupsFromDisk()
	go func() {
		if _, err := a.client.GetUserGroups(); err != nil {
			slog.Warn("load usergroups failed", "error", err)
			return
		}
		a.w.Invalidate()
	}()

	tick := func() {
		a.startTask("channels", "Syncing channels")
		defer a.endTask("channels")
		channels, err := a.client.GetChannels(a.cfg.Channels.Types, a.cfg.Channels.Pinned)
		if err != nil {
			slog.Error("GetChannels failed", "error", err)
			return
		}
		// Filter hidden.
		hidden := make(map[string]bool, len(a.cfg.Channels.Hidden))
		for _, id := range a.cfg.Channels.Hidden {
			hidden[id] = true
		}
		filtered := channels[:0]
		for _, ch := range channels {
			if !hidden[ch.ID] {
				filtered = append(filtered, ch)
			}
		}
		a.channels.SetChannels(filtered)
		a.autoSelectFirst(filtered)
		a.w.Invalidate()

		// IM and MPIM names aren't fully resolved by GetChannels; do it in the
		// background so the sidebar appears immediately, then refresh once names
		// are in.
		go func(chs []slack.Channel) {
			a.startTask("resolve", "Resolving names")
			defer a.endTask("resolve")
			a.client.ResolveConversationNames(chs)
			a.channels.SetChannels(chs)
			a.w.Invalidate()
		}(filtered)
	}

	tick()
	t := time.NewTicker(a.cfg.Polling.ChannelList)
	pt := time.NewTicker(a.cfg.Polling.Priority)
	defer t.Stop()
	defer pt.Stop()
	for {
		select {
		case <-t.C:
			tick()
		case <-pt.C:
			// Refresh unread counts for priority channels
			activeID := a.getActiveID()
			priority := make([]string, 0, len(a.cfg.Channels.Pinned)+2)

			if activeID == "__UNREADS__" {
				// When looking at All Unreads, we need to know if ANY channel has new messages
				for _, ch := range a.client.Cache().GetAllChannels() {
					priority = append(priority, ch.ID)
				}
			} else {
				priority = append(priority, a.cfg.Channels.Pinned...)
				if activeID != "" {
					priority = append(priority, activeID)
				}
				// Include channels that currently have unreads so they stay accurate
				for _, ch := range a.client.Cache().GetAllChannels() {
					if ch.UnreadCount > 0 {
						priority = append(priority, ch.ID)
					}
				}
			}

			if len(priority) > 0 {
				go func(ids []string) {
					a.startTask("priority", "Updating unreads")
					defer a.endTask("priority")
					channels, err := a.client.GetUnreadCounts(ids)
					if err != nil {
						slog.Debug("GetUnreadCounts failed", "error", err)
						return
					}

					// Perform background mention scan for any channel that has unreads
					for _, ch := range channels {
						if ch.UnreadCount > 0 {
							msgs, err := a.client.GetMessages(ch.ID, 50, ch.LastReadTS)
							if err == nil {
								mentions := 0
								for _, m := range msgs {
									if a.isMention(m) {
										mentions++
									}
								}
								if cached := a.client.Cache().GetChannel(ch.ID); cached != nil {
									if cached.MentionCount != mentions {
										a.client.Cache().SetChannelUnread(ch.ID, ch.UnreadCount, mentions, ch.LastReadTS, ch.LatestTS)
									}
								}
							}
						}
					}

					// Update the sidebar
					a.channels.SetChannels(a.client.Cache().GetAllChannels())
					a.w.Invalidate()

					// If we're in the All Unreads view, trigger a full refresh
					if a.getActiveID() == "__UNREADS__" {
						a.fetchAllUnreads()
					}
				}(priority)
			}
		}
	}
}

// forceRefresh re-fetches whatever the user is currently looking at: the
// active thread (if open), the unreads aggregate, or the active channel.
// It also re-syncs the channel list so unread badges update.
func (a *App) forceRefresh() {
	go func() {
		_, _ = a.client.GetChannels(a.cfg.Channels.Types, a.cfg.Channels.Pinned)
		a.w.Invalidate()
	}()
	if a.messages.InThread() {
		chID, threadTS := a.messages.ThreadInfo()
		if chID != "" && threadTS != "" {
			go a.fetchThread(chID, threadTS)
		}
	}
	id := a.getActiveID()
	switch id {
	case "":
		return
	case "__UNREADS__":
		go a.fetchAllUnreads()
	default:
		go a.fetchMessages(id)
	}
}

// pollActiveChannel re-fetches messages for the currently active channel on
// the configured cadence.
func (a *App) pollActiveChannel() {
	t := time.NewTicker(a.cfg.Polling.ActiveChannel)
	defer t.Stop()
	for range t.C {
		id := a.getActiveID()
		if id == "" {
			continue
		}
		if id == "__UNREADS__" {
			a.fetchAllUnreads()
		} else {
			a.fetchMessages(id)
		}
	}
}

func (a *App) fetchMessages(id string) {
	a.startTask("fetch:"+id, "Fetching messages")
	defer a.endTask("fetch:" + id)
	limit := a.cfg.Display.MessageLimit
	if limit <= 0 {
		limit = 50
	}
	msgs, err := a.client.GetMessages(id, limit, "")
	if err != nil {
		slog.Error("GetMessages failed", "channel", id, "error", err)
		return
	}

	ch := a.client.Cache().GetChannel(id)
	if ch == nil {
		return
	}

	mentionsFound := 0
	for _, m := range msgs {
		if m.Timestamp > ch.LastReadTS && a.isMention(m) {
			mentionsFound++
		}
	}

	// Update LatestTS and MentionCount in cache if we found newer messages
	changed := false
	var lastTS string
	if len(msgs) > 0 {
		lastTS = msgs[len(msgs)-1].Timestamp
		if lastTS > ch.LatestTS {
			a.client.Cache().AdvanceChannelLatestTS(id, lastTS)
			changed = true
			// Refresh our local copy of ch to have the updated LatestTS
			ch = a.client.Cache().GetChannel(id)
		}
	}

	isActive := a.getActiveID() == id && !a.viewingContext
	if isActive && lastTS != "" && (ch.UnreadCount > 0 || lastTS > ch.LastReadTS) {
		a.client.Cache().SetChannelUnread(id, 0, 0, lastTS, lastTS)
		changed = true
		go func(channelID, timestamp string) {
			if err := a.client.MarkChannel(channelID, timestamp); err != nil {
				slog.Debug("MarkChannel failed", "channel", channelID, "error", err)
			}
		}(id, lastTS)
	} else if !isActive && ch.MentionCount != mentionsFound {
		a.client.Cache().SetChannelUnread(id, ch.UnreadCount, mentionsFound, ch.LastReadTS, ch.LatestTS)
		changed = true
	}

	if changed {
		a.channels.SetChannels(a.client.Cache().GetAllChannels())
	}

	if a.getActiveID() == id && !a.viewingContext {
		a.messages.SetMessages(msgs)
		a.w.Invalidate()
	}
}

func (a *App) fetchMessagesAround(id string, ts string) {
	a.startTask("fetch:"+id, "Fetching messages around context")
	defer a.endTask("fetch:" + id)
	limit := a.cfg.Display.MessageLimit
	if limit <= 0 {
		limit = 50
	}
	msgs, err := a.client.GetMessagesContext(id, limit, ts)
	if err != nil {
		slog.Error("GetMessagesContext failed", "channel", id, "error", err)
		return
	}

	if a.getActiveID() == id {
		a.messages.SetMessages(msgs)
		a.w.Invalidate()
	}
}

func (a *App) fetchAllUnreads() {
	a.startTask("unreads", "Fetching unreads")
	defer a.endTask("unreads")
	var all []slack.Message
	channels := a.client.Cache().GetAllChannels()

	for _, ch := range channels {
		if ch.UnreadCount > 0 {
			msgs, err := a.client.GetMessages(ch.ID, 100, ch.LastReadTS)
			if err != nil {
				slog.Error("fetchAllUnreads failed", "channel", ch.ID, "error", err)
				continue
			}
			mentionsFound := 0
			for i := range msgs {
				isDM := ch.IsIM || ch.IsMPIM
				if isDM || a.isMention(msgs[i]) {
					msgs[i].ChannelID = ch.ID
					msgs[i].ChannelName = ch.Name
					all = append(all, msgs[i])
					mentionsFound++
				}
			}
			// Update the cache with the actual mention count we found
			if cached := a.client.Cache().GetChannel(ch.ID); cached != nil {
				if cached.MentionCount != mentionsFound {
					a.client.Cache().SetChannelUnread(ch.ID, cached.UnreadCount, mentionsFound, cached.LastReadTS, cached.LatestTS)
				}
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp < all[j].Timestamp
	})
	if a.getActiveID() == "__UNREADS__" {
		a.messages.SetMessages(all)
		a.w.Invalidate()
	}
}

func (a *App) handleClipboardEvents(gtx layout.Context) {
	// Catch all types to log what's actually coming in
	for {
		ev, ok := gtx.Source.Event(transfer.TargetFilter{Target: &a.composerPasteTag})
		if !ok {
			break
		}
		slog.Info("clipboard event received", "type", fmt.Sprintf("%T", ev))
		if dev, ok := ev.(transfer.DataEvent); ok {
			slog.Info("clipboard data event", "mime", dev.Type)
			r := dev.Open()
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				slog.Error("clipboard read failed", "type", dev.Type, "error", err)
				continue
			}
			slog.Info("clipboard data read", "size", len(data))

			if strings.HasPrefix(dev.Type, "image/") {
				ext := "png"
				if strings.Contains(dev.Type, "jpeg") {
					ext = "jpg"
				} else if strings.Contains(dev.Type, "gif") {
					ext = "gif"
				} else if strings.Contains(dev.Type, "bmp") {
					ext = "bmp"
				} else if strings.Contains(dev.Type, "tiff") {
					ext = "tiff"
				}
				filename := fmt.Sprintf("pasted_image_%s.%s", time.Now().Format("20060102_150405"), ext)
				a.composer.AddAttachment(filename, data)
				a.w.Invalidate()
				return
			} else if dev.Type == "text/uri-list" {
				lines := strings.Split(string(data), "\r\n")
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					// file:///path/to/file
					path := strings.TrimPrefix(line, "file://")
					fileData, err := os.ReadFile(path)
					if err != nil {
						slog.Error("read pasted file failed", "path", path, "error", err)
						continue
					}
					a.composer.AddAttachment(filepath.Base(path), fileData)
				}
				a.w.Invalidate()
				return
			} else if dev.Type == "text/plain" || dev.Type == "text/plain;charset=utf-8" || dev.Type == "UTF8_STRING" {
				a.composer.editor.Insert(string(data))
			}
		}
	}
}

func (a *App) onAttach() {
	go func() {
		const sep = "|||"
		// Try zenity first
		cmd := exec.Command("zenity", "--file-selection", "--multiple", "--separator="+sep, "--title=Select Files to Attach")
		out, err := cmd.Output()
		if err != nil {
			// Fallback to kdialog
			cmd = exec.Command("kdialog", "--getopenfilename", ".", "--multiple", "--separate-output")
			out, err = cmd.Output()
		}
		if err != nil {
			slog.Debug("file picker failed or cancelled", "error", err)
			return
		}

		output := strings.TrimSpace(string(out))
		if output == "" {
			return
		}

		var paths []string
		if strings.Contains(output, sep) {
			paths = strings.Split(output, sep)
		} else {
			// kdialog --multiple --separate-output uses newlines
			paths = strings.Split(output, "\n")
		}

		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				slog.Error("read file failed", "path", path, "error", err)
				continue
			}

			a.composer.AddAttachment(filepath.Base(path), data)
		}
		a.w.Invalidate()
	}()
}

func (a *App) tryWaylandPaste() bool {
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}

	// Try wl-paste
	cmd := exec.Command("wl-paste", "--list-types")
	out, err := cmd.Output()
	if err != nil {
		return false
	}

	types := strings.Split(strings.TrimSpace(string(out)), "\n")
	typeMap := make(map[string]bool)
	for _, t := range types {
		typeMap[t] = true
	}

	// Priority to images
	imageTypes := []string{"image/png", "image/jpeg", "image/gif", "image/bmp", "image/tiff"}
	for _, mime := range imageTypes {
		if typeMap[mime] {
			data, err := exec.Command("wl-paste", "--type", mime).Output()
			if err == nil {
				ext := "png"
				if strings.Contains(mime, "jpeg") {
					ext = "jpg"
				} else if strings.Contains(mime, "gif") {
					ext = "gif"
				} else if strings.Contains(mime, "bmp") {
					ext = "bmp"
				} else if strings.Contains(mime, "tiff") {
					ext = "tiff"
				}
				filename := fmt.Sprintf("pasted_image_%s.%s", time.Now().Format("20060102_150405"), ext)
				a.composer.AddAttachment(filename, data)
				a.w.Invalidate()
				return true
			}
		}
	}

	// Fallback to text
	if typeMap["text/plain"] || typeMap["UTF8_STRING"] {
		data, err := exec.Command("wl-paste").Output()
		if err == nil {
			a.composer.editor.Insert(string(data))
			a.w.Invalidate()
			return true
		}
	}

	return false
}
