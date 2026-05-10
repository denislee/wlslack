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
	// Bare struct{} is fine -- we only use its address.
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

	editingTS string
	editingCh string

	pendingG bool

	lastFullUnreadRefresh time.Time
	// rotatingScanIdx walks the channel list one slice at a time on each
	// priority tick. Combined with the priority set (pinned/active/recent),
	// this gives every channel a refresh within a few minutes without
	// blocking on a giant full sweep.
	rotatingScanIdx int

	fetchingUnreads atomic.Bool

	// sidebarPublish coalesces sidebar updates so multiple concurrent
	// cache mutations (priority tick, mention scans, active-channel poll)
	// produce a single republish instead of flickering through intermediate
	// orderings. Buffered with size 1 so signals are dropped while a
	// publish is already pending.
	sidebarPublish chan struct{}
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
		w:              w,
		th:             newTheme(),
		client:         client,
		cfg:            cfg,
		fmt:            slack.NewFormatter(client.Cache(), cfg.Display.TimestampFormat),
		sidebarPublish: make(chan struct{}, 1),
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
	a.th.ShowUnreadOnCollapse = state.ShowUnreadOnCollapse
	a.th.ShowStatusBar = state.ShowStatusBar
	a.th.DisableLinkUnfurl = state.DisableLinkUnfurl
	a.th.DisableMediaUnfurl = state.DisableMediaUnfurl
	a.client.SetUnfurlSettings(a.th.DisableLinkUnfurl, a.th.DisableMediaUnfurl)
	a.backgroundTasks = make(map[string]string)
	a.channels.SetFavorites(state.Favorites, a.onFavoritesChanged)
	a.channels.SetCollapsedGroups(state.CollapsedGroups, a.onCollapsedGroupsChanged)
	a.channels.SetHidden(a.cfg.Channels.Hidden)
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
	a.client.Cache().OnUpdate = a.requestSidebarPublish
	go a.runSidebarPublisher()

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
					// left the editor -- checked via gtx.Source.Focused below.
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
							if a.editingTS != "" {
								placeholder = "Edit message"
							} else if id := a.getActiveID(); id != "" {
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
		filters = append(filters, a.composer.KeyFilters()...)
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
			key.Filter{Name: "E", Optional: key.ModShift},
			key.Filter{Name: "D", Optional: key.ModShift},
			key.Filter{Name: "Y", Optional: key.ModShift},
			key.Filter{Name: "R", Optional: key.ModShift},
			key.Filter{Name: "G", Optional: key.ModShift},
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

		wasPendingG := a.pendingG
		a.pendingG = false

		switch {
		case kev.Name == "G" && !composerFocused:
			delta := 1000000
			if !kev.Modifiers.Contain(key.ModShift) {
				if wasPendingG {
					delta = -1000000
				} else {
					a.pendingG = true
					continue
				}
			}
			switch {
			case a.linkPickerOpen:
				a.linkPicker.MoveSelection(delta)
				a.w.Invalidate()
			case a.imageViewerOpen:
				a.imageViewer.MoveSelection(delta)
				a.w.Invalidate()
			default:
				a.moveInPane(delta)
			}
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
				if a.composer.HandleKey(gtx, kev, a.onSend) {
					a.w.Invalidate()
				} else {
					// HandleKey returns false in Normal mode for Esc, so we exit focus.
					a.editingTS = ""
					a.editingCh = ""
					a.pendFocusKeyTag = true
					a.w.Invalidate()
				}
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
		case composerFocused:
			if a.composer.HandleKey(gtx, kev, a.onSend) {
				a.w.Invalidate()
				continue
			}

			// App-level composer shortcuts that need App state or special gtx commands
			switch {
			case (kev.Name == "V" || kev.Name == "v") && kev.Modifiers.Contain(key.ModCtrl):
				slog.Info("Ctrl+V detected", "tag", fmt.Sprintf("%p", &a.composerPasteTag))
				if !a.tryWaylandPaste() {
					gtx.Execute(clipboard.ReadCmd{Tag: &a.composerPasteTag})
				}
			case kev.Name == "T" && kev.Modifiers.Contain(key.ModCtrl):
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
			}
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
			if kev.Modifiers.Contain(key.ModShift) {
				if a.focusPane == paneMessages {
					if a.messages.OpenAuthor(a.fmt) {
						a.w.Invalidate()
					}
				}
				break
			}
			// h peels back one layer at a time: link picker > author panel >
			// thread > channels-pane focus. This keeps h/l symmetrical with the
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
			//   channel-history selection > thread view
			//   thread selection         > author detail panel
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
				a.openMessageEditor(gtx)
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
				msg, ts, ok := a.messages.SelectedMessage()
				if ok {
					var text string
					if kev.Modifiers.Contain(key.ModShift) {
						chID := a.getActiveID()
						if chID == "__UNREADS__" && msg.ChannelID != "" {
							chID = msg.ChannelID
						}
						text = a.client.Permalink(chID, ts)
					} else {
						text = a.fmt.Format(msg.Text)
						for _, f := range msg.Files {
							if text != "" {
								text += "\n"
							}
							text += f.PreferredImageURL()
						}
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
			// R on the channels pane triggers a full refresh/poll.
			if a.focusPane == paneChannels {
				go a.pollChannels()
				break
			}
			// Shift differentiates the two reaction shortcuts: capital R
			// echoes every reaction already on the message, lowercase r
			// opens the picker so the user can choose a new one.
			if kev.Modifiers.Contain(key.ModShift) {
				a.echoReactions()
			} else {
				a.openReactionPicker()
			}
		case kev.Name == "I":
			a.composer.SetInsertMode()
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
//   - 1 link > open in browser
//   - >1 links > link picker
//   - 0 links and >1 images > in-app image viewer
//   - everything else > no-op (single inline image already shows in the row)
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

func (a *App) openMessageEditor(gtx layout.Context) {
	msg, ts, ok := a.messages.SelectedMessage()
	if !ok {
		return
	}

	// If it's my message, edit it in the composer instead of opening the
	// read-only full-screen editor.
	if msg.UserID == a.client.GetSelfID() {
		chID := a.getActiveID()
		if chID == "__UNREADS__" && msg.ChannelID != "" {
			chID = msg.ChannelID
		}
		if chID == "" {
			return
		}
		a.editingTS = ts
		a.editingCh = chID
		a.composer.SetPendingText(msg.Text)
		a.composer.SetInsertMode()
		a.composerVisible = true
		a.composerWasFocused = false
		gtx.Execute(key.FocusCmd{Tag: &a.composer.editor})
		a.w.Invalidate()
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
	if (chID == "__UNREADS__" || chID == "__THREADS__") && msg.ChannelID != "" {
		chID = msg.ChannelID
	}
	if chID == "" || strings.HasPrefix(chID, "__") {
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
	if (chID == "__UNREADS__" || chID == "__THREADS__") && msg.ChannelID != "" {
		chID = msg.ChannelID
	}
	if chID == "" || strings.HasPrefix(chID, "__") {
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
		// Single refresh after all operations are done.
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
			go a.fetchThread(chID, threadTS)
			return
		}
	}

	id := a.getActiveID()
	switch id {
	case "__UNREADS__":
		go a.fetchAllUnreads()
	case "__THREADS__":
		go a.fetchAllThreads()
	case chID:
		go a.fetchMessages(chID)
	default:
		// If we're looking at a different channel than where the reaction
		// landed, there's no need to refresh the messages pane, but we might
		// want to refresh the sidebar if unread counts changed.
		// For now, doing nothing is safe.
	}
	a.w.Invalidate()
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
		a.refreshView(chID)
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
		a.refreshView(ch)
		a.w.Invalidate()
	}()
}

func (a *App) refreshView(chID string) {
	id := a.getActiveID()
	switch id {
	case "__UNREADS__":
		go a.fetchAllUnreads()
	case "__THREADS__":
		go a.fetchAllThreads()
	case chID:
		go a.fetchMessages(chID)
	}
}

// openURL hands a URL to the system browser via xdg-open. Errors are logged
// but not surfaced -- the user just sees no browser pop, which is consistent
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
	state.ShowUnreadOnCollapse = a.th.ShowUnreadOnCollapse
	state.ShowStatusBar = a.th.ShowStatusBar
	state.DisableLinkUnfurl = a.th.DisableLinkUnfurl
	state.DisableMediaUnfurl = a.th.DisableMediaUnfurl
	config.SaveUIState(state)
	a.client.SetUnfurlSettings(a.th.DisableLinkUnfurl, a.th.DisableMediaUnfurl)
	a.w.Invalidate()
}

// prefsFromState bridges config.FontPrefs > ui sectionPrefs without making
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
// panel takes priority -- when it's open, j/k walks the field list rather than
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
	a.editingTS = ""
	a.editingCh = ""
	if id == "__UNREADS__" {
		a.messages.SetHeader("Mentions", "Consolidated view of all unread messages with mentions")
		a.messages.SetMessages(nil)
		a.w.Invalidate()

		priority := make([]string, 0, len(a.cfg.Channels.Pinned)+len(a.cfg.Channels.Hidden))
		priority = append(priority, a.cfg.Channels.Pinned...)
		priority = append(priority, a.cfg.Channels.Hidden...)
		for _, ch := range a.client.Cache().GetAllChannels() {
			if ch.UnreadCount > 0 {
				priority = append(priority, ch.ID)
			}
		}
		go a.scanUnreads(priority)
		return
	}
	if id == "__THREADS__" {
		a.messages.SetHeader("Threads", "Recent threads you are part of")
		a.messages.SetMessages(nil)
		a.w.Invalidate()
		go a.fetchAllThreads()
		return
	}
	if ch := a.client.Cache().GetChannel(id); ch != nil {
		a.messages.SetHeader(ch.Name, ch.Topic)
		if ch.UnreadCount > 0 && ch.LatestTS != "" {
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

	editingTS := a.editingTS
	editingCh := a.editingCh
	a.editingTS = ""
	a.editingCh = ""

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
			if editingTS != "" {
				a.startTask("edit", "Editing message")
				defer a.endTask("edit")
				err := a.client.UpdateMessage(editingCh, editingTS, text)
				if err != nil {
					slog.Error("edit failed", "channel", editingCh, "ts", editingTS, "error", err)
					return
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
		}

		// Refetch to surface the message.
		if threadTS != "" {
			a.fetchThread(id, threadTS)
		} else {
			a.refreshView(id)
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
	// different one -- otherwise we'd silently overwrite their current view.
	if !a.messages.InThread() {
		return
	}
	curCh, curTS := a.messages.ThreadInfo()
	if curCh != channelID || curTS != threadTS {
		return
	}
	if a.messages.SetThreadMessages(msgs) {
		a.w.Invalidate()
	}
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
	return a.isMentionWithContext(msg, selfID, groups)
}

func (a *App) isMentionWithContext(msg slack.Message, selfID string, groups []slack.UserGroup) bool {
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

func (a *App) updateChannelsSidebar(channels []slack.Channel) ([]slack.Channel, bool) {
	hidden := make(map[string]bool, len(a.cfg.Channels.Hidden))
	for _, id := range a.cfg.Channels.Hidden {
		hidden[id] = true
	}
	filtered := make([]slack.Channel, 0, len(channels))
	for _, ch := range channels {
		if !hidden[ch.ID] || ch.UnreadCount > 0 || ch.MentionCount > 0 {
			filtered = append(filtered, ch)
		}
	}
	changed := a.channels.SetChannels(filtered)
	return filtered, changed
}

// requestSidebarPublish signals the publisher that the cache has changed
// and the sidebar should be re-rendered. Multiple signals received during
// the debounce window collapse into a single republish, so callers from
// different goroutines can mutate the cache in parallel without showing
// intermediate orderings.
func (a *App) requestSidebarPublish() {
	select {
	case a.sidebarPublish <- struct{}{}:
	default:
	}
}

// runSidebarPublisher serves sidebar publish requests on a single goroutine.
// After the first signal it waits for activity to settle, draining further
// signals during the window so a burst of cache mutations only triggers one
// SetChannels call. The result is a single visible reorder per refresh
// cycle instead of a flicker through every intermediate state.
func (a *App) runSidebarPublisher() {
	const debounce = 150 * time.Millisecond
	for range a.sidebarPublish {
		timer := time.NewTimer(debounce)
		settling := true
		for settling {
			select {
			case <-a.sidebarPublish:
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(debounce)
			case <-timer.C:
				settling = false
			}
		}
		_, changed := a.updateChannelsSidebar(a.client.Cache().GetAllChannels())
		if changed {
			a.w.Invalidate()
		}
	}
}

func (a *App) pollChannels() {
	// One-shot disk-cache load so the sidebar paints immediately on startup
	// instead of staring at an empty pane until the first GetChannels round
	// trip returns. Subsequent refreshes always come from the live API.
	if cached, err := a.client.Cache().LoadChannelsFromDisk(); err == nil && len(cached) > 0 {
		filtered, changed := a.updateChannelsSidebar(cached)
		a.autoSelectFirst(filtered)
		if changed {
			a.w.Invalidate()
		}
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
		favs := a.channels.Favorites()
		priority := make([]string, 0, len(a.cfg.Channels.Pinned)+len(a.cfg.Channels.Hidden)+len(favs))
		priority = append(priority, a.cfg.Channels.Pinned...)
		priority = append(priority, a.cfg.Channels.Hidden...)
		priority = append(priority, favs...)
		channels, err := a.client.GetChannels(a.cfg.Channels.Types, priority)
		if err != nil {
			slog.Error("GetChannels failed", "error", err)
			return
		}
		// Resolve IM/MPIM names before publishing so the sidebar reflects the
		// final names (sort is by activity then name; missing names would
		// cause a visible re-order once they arrived).
		a.client.ResolveConversationNames(channels)
		// Route the publish through the debouncer instead of calling
		// SetChannels directly. The priority tick and active-channel poll
		// also signal the publisher; coalescing them under one debounce
		// window means the user sees a single republish per refresh cycle
		// instead of tick()'s ordering flashing past before the cached
		// state catches up.
		a.requestSidebarPublish()
		a.autoSelectFirst(channels)
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
			if a.client.IsClientCountsSupported() {
				go a.refreshUnreadsAggregate()
				continue
			}

			// Per-channel fallback (xoxp/xoxb without users.counts). Tier 3
			// limits us to ~50 conversations.info calls per minute, so we
			// build a small set per tick:
			//   - the obvious priority set (pinned/hidden/favorites/active/
			//     already-unread/last-hour-active)
			//   - plus a rolling slice of the workspace, so quiet channels
			//     get refreshed in a steady cycle instead of one giant sweep
			//     that monopolises the lock for minutes.
			activeID := a.getActiveID()
			now := time.Now()

			seen := make(map[string]bool)
			priority := make([]string, 0, 64)
			add := func(id string) {
				if id == "" || seen[id] {
					return
				}
				seen[id] = true
				priority = append(priority, id)
			}

			for _, id := range a.cfg.Channels.Pinned {
				add(id)
			}
			for _, id := range a.cfg.Channels.Hidden {
				add(id)
			}
			for _, id := range a.channels.Favorites() {
				add(id)
			}
			add(activeID)
			recentCutoff := fmt.Sprintf("%d.000000", now.Add(-1*time.Hour).Unix())
			all := a.client.Cache().GetAllChannels()
			for _, ch := range all {
				if ch.UnreadCount > 0 || (ch.LatestTS != "" && ch.LatestTS >= recentCutoff) {
					add(ch.ID)
				}
			}

			// Rolling slice through the rest of the workspace. windowSize is
			// sized so the scan finishes well within the 5s tick under Tier 3
			// limits — at ~50 calls/min sustained, 10 channels takes ~12s with
			// retries, but most aren't rate-limited so the typical case is
			// 2–3s. Workspace cycle time = len(all)/windowSize * 5s.
			const windowSize = 10
			rotated := 0
			for i := 0; i < len(all) && rotated < windowSize; i++ {
				idx := (a.rotatingScanIdx + i) % len(all)
				if !seen[all[idx].ID] {
					add(all[idx].ID)
					rotated++
				}
			}
			if len(all) > 0 {
				a.rotatingScanIdx = (a.rotatingScanIdx + windowSize) % len(all)
			}

			slog.Info("priority tick", "channels", len(priority), "rotated", rotated, "total_workspace", len(all))
			if len(priority) > 0 {
				go a.scanUnreads(priority)
			}
		}
	}
}

func (a *App) scanUnreads(ids []string) {
	if a.fetchingUnreads.Swap(true) {
		slog.Info("scanUnreads skipped (busy)", "requested", len(ids))
		return
	}
	defer a.fetchingUnreads.Store(false)

	a.startTask("priority", "Updating unreads")
	defer a.endTask("priority")

	start := time.Now()
	slog.Info("scanUnreads start", "channels", len(ids))
	channels, err := a.client.GetUnreadCounts(ids)
	if err != nil {
		slog.Warn("GetUnreadCounts failed", "error", err)
		return
	}
	withUnread := 0
	for _, ch := range channels {
		if ch.UnreadCount > 0 {
			withUnread++
		}
	}
	slog.Info("scanUnreads done", "requested", len(ids), "got", len(channels), "with_unread", withUnread, "elapsed", time.Since(start).String())

	a.scanMentions(channels)
	a.requestSidebarPublish()

	switch a.getActiveID() {
	case "__UNREADS__":
		a.fetchAllUnreads()
	case "__THREADS__":
		go a.fetchAllThreads()
	}
}

// refreshUnreadsAggregate is the xoxc-only fast path: one client.counts call
// updates unread/mention state for every cached channel, then a per-channel
// mention scan runs only on those that have unreads to keep the red-badge
// counts accurate. This replaces the slow per-channel conversations.info
// rotation when it's available.
func (a *App) refreshUnreadsAggregate() {
	if a.fetchingUnreads.Swap(true) {
		return
	}
	defer a.fetchingUnreads.Store(false)

	a.startTask("priority", "Updating unreads")
	defer a.endTask("priority")

	counts, err := a.client.GetClientCounts()
	if err != nil {
		slog.Warn("client.counts failed, will retry next tick", "error", err)
		return
	}
	a.client.ApplyClientCounts(counts)
	applied := 0
	for _, ch := range a.client.Cache().GetAllChannels() {
		if ch.UnreadCount > 0 {
			applied++
		}
	}
	slog.Info("client.counts applied", "fetched", len(counts), "now_unread", applied)

	unread := make([]slack.Channel, 0)
	for _, ch := range a.client.Cache().GetAllChannels() {
		if ch.UnreadCount > 0 {
			unread = append(unread, ch)
		}
	}
	a.scanMentions(unread)
	a.requestSidebarPublish()

	switch a.getActiveID() {
	case "__UNREADS__":
		a.fetchAllUnreads()
	case "__THREADS__":
		go a.fetchAllThreads()
	}
}

// scanMentions fetches recent messages for every channel that has unreads and
// recomputes the mention badge from message text. Slack's aggregate counts
// don't always agree with our per-message detection (group mentions, @here
// scoping rules, etc.), so we always overwrite with the locally-computed
// value to keep the badge consistent with the Mentions view.
func (a *App) scanMentions(channels []slack.Channel) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	selfID := a.client.GetSelfID()
	groups := a.client.Cache().GetAllUserGroups()

	for _, ch := range channels {
		if ch.UnreadCount == 0 {
			continue
		}
		wg.Add(1)
		go func(ch slack.Channel) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			limit := 50
			if a.getActiveID() == "__UNREADS__" {
				limit = 100
			}

			msgs, err := a.client.GetMessages(ch.ID, limit, ch.LastReadTS)
			if err != nil {
				return
			}
			localUnread := 0
			mentions := 0
			isDM := ch.IsIM || ch.IsMPIM
			for _, m := range msgs {
				if ch.LastReadTS != "" && m.Timestamp <= ch.LastReadTS {
					continue
				}
				localUnread++
				if a.isMentionWithContext(m, selfID, groups) {
					mentions++
				}
			}
			cached := a.client.Cache().GetChannel(ch.ID)
			if cached == nil {
				return
			}
			// client.counts only returns has_unreads (boolean), so ApplyClientCounts
			// pins UnreadCount at 1 as a placeholder. Refine it with the locally
			// counted messages newer than LastReadTS — this is the same window
			// scanMentions already fetches, so no extra API calls. Trust the
			// higher of (cached, local) so a precise count from conversations.info
			// (xoxp/xoxb path, possibly larger than our limit) isn't truncated.
			unread := cached.UnreadCount
			if cached.UnreadCount <= 1 || localUnread > cached.UnreadCount {
				unread = localUnread
			}
			if isDM {
				mentions = unread
			}
			if cached.MentionCount != mentions || cached.UnreadCount != unread {
				a.client.Cache().SetChannelUnread(ch.ID, unread, mentions, ch.LastReadTS, ch.LatestTS)
				a.w.Invalidate()
			}
		}(ch)
	}
	wg.Wait()
}

// forceRefresh re-fetches whatever the user is currently looking at: the
// active thread (if open), the unreads aggregate, or the active channel.
// It also re-syncs the channel list so unread badges update.
func (a *App) forceRefresh() {
	id := a.getActiveID()
	if id == "__UNREADS__" {
		priority := make([]string, 0, len(a.cfg.Channels.Pinned)+len(a.cfg.Channels.Hidden))
		priority = append(priority, a.cfg.Channels.Pinned...)
		priority = append(priority, a.cfg.Channels.Hidden...)
		for _, ch := range a.client.Cache().GetAllChannels() {
			if ch.UnreadCount > 0 {
				priority = append(priority, ch.ID)
			}
		}
		go a.scanUnreads(priority)
		return
	}

	go func() {
		priority := make([]string, 0, len(a.cfg.Channels.Pinned)+len(a.cfg.Channels.Hidden))
		priority = append(priority, a.cfg.Channels.Pinned...)
		priority = append(priority, a.cfg.Channels.Hidden...)
		_, err := a.client.GetChannels(a.cfg.Channels.Types, priority)
		if err == nil {
			a.requestSidebarPublish()
		}
	}()
	if a.messages.InThread() {
		chID, threadTS := a.messages.ThreadInfo()
		if chID != "" && threadTS != "" {
			go a.fetchThread(chID, threadTS)
		}
	}
	switch id {
	case "":
		return
	case "__THREADS__":
		go a.fetchAllThreads()
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
		switch id {
		case "":
			continue
		case "__UNREADS__":
			a.fetchAllUnreads()
		case "__THREADS__":
			a.fetchAllThreads()
		default:
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
	isDM := ch.IsIM || ch.IsMPIM
	selfID := a.client.GetSelfID()
	groups := a.client.Cache().GetAllUserGroups()
	for _, m := range msgs {
		if m.Timestamp > ch.LastReadTS && a.isMentionWithContext(m, selfID, groups) {
			mentionsFound++
		}
	}
	if isDM {
		mentionsFound = ch.UnreadCount
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
		go func(channelID, timestamp string) {
			if err := a.client.MarkChannel(channelID, timestamp); err != nil {
				slog.Debug("MarkChannel failed", "channel", channelID, "error", err)
			}
		}(id, lastTS)
	} else if !isActive && ch.MentionCount != mentionsFound {
		a.client.Cache().SetChannelUnread(id, ch.UnreadCount, mentionsFound, ch.LastReadTS, ch.LatestTS)
	}

	if changed {
		a.requestSidebarPublish()
	}

	if a.getActiveID() == id && !a.viewingContext {
		if a.messages.SetMessages(msgs) {
			a.w.Invalidate()
		}
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
		if a.messages.SetMessages(msgs) {
			a.w.Invalidate()
		}
	}
}

func (a *App) fetchAllUnreads() {
	var all []slack.Message
	channels := a.client.Cache().GetAllChannels()
	selfID := a.client.GetSelfID()
	groups := a.client.Cache().GetAllUserGroups()

	for _, ch := range channels {
		if ch.UnreadCount > 0 {
			msgs := a.client.Cache().GetMessages(ch.ID)
			isDM := ch.IsIM || ch.IsMPIM
			for _, m := range msgs {
				if m.Timestamp > ch.LastReadTS && (isDM || a.isMentionWithContext(m, selfID, groups)) {
					m.ChannelID = ch.ID
					m.ChannelName = ch.Name
					all = append(all, m)
				}
			}
		}
	}

	// Also include unread threads from search.
	if results, err := a.client.Search("has:thread OR (is:dm has:thread)"); err == nil {
		for _, r := range results {
			ch := a.client.Cache().GetChannel(r.ChannelID)
			if ch == nil {
				continue
			}
			isDM := ch.IsIM || ch.IsMPIM
			isMention := a.isMention(r.Message)
			if (isDM || isMention) && r.Message.LastReplyTS > ch.LastReadTS {
				// Avoid duplicates
				found := false
				for _, m := range all {
					if m.Timestamp == r.Message.Timestamp && m.ChannelID == r.ChannelID {
						found = true
						break
					}
				}
				if !found {
					m := r.Message
					m.ChannelID = r.ChannelID
					m.ChannelName = r.ChannelName
					all = append(all, m)
				}
			}
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Timestamp < all[j].Timestamp
	})
	if a.getActiveID() == "__UNREADS__" {
		if a.messages.SetMessages(all) {
			a.w.Invalidate()
		}
	}
}

func (a *App) fetchAllThreads() {
	a.startTask("threads", "Fetching threads")
	defer a.endTask("threads")
	// Note: Slack's search API is used as a fallback since there's no direct "all threads" API.
	results, err := a.client.Search("has:thread OR (is:dm has:thread)")
	if err != nil {
		slog.Error("fetchAllThreads failed", "error", err)
		return
	}
	var msgs []slack.Message
	for _, r := range results {
		m := r.Message
		m.ChannelID = r.ChannelID
		m.ChannelName = r.ChannelName
		// Search results for "has:thread" are by definition thread roots (or
		// replies, but we treat them as roots). UI needs ReplyCount > 0 to
		// show the thread indicator.
		m.ReplyCount = 1
		if m.ThreadTS == "" {
			m.ThreadTS = m.Timestamp
		}
		msgs = append(msgs, m)
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].Timestamp > msgs[j].Timestamp
	})
	if a.getActiveID() == "__THREADS__" {
		if a.messages.SetMessages(msgs) {
			a.w.Invalidate()
		}
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
