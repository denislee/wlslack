package ui

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/app"
	"gioui.org/io/clipboard"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/user/wlslack/internal/config"
	"github.com/user/wlslack/internal/slack"
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
	reactionPicker *ReactionPicker
	settings       *SettingsScreen

	mu          sync.Mutex
	channelList []slack.Channel

	activeID atomic.Value // string

	// keyTag is the focus target for app-level shortcuts (j/k navigation).
	// Bare struct{} is fine — we only use its address.
	keyTag struct{}

	// UI mode flags. Mutated from goroutines, read on the UI thread; the
	// reads happen during Layout while writes trigger Invalidate, so a frame
	// boundary acts as the sync point.
	switcherOpen        bool
	linkPickerOpen      bool
	imageViewerOpen     bool
	reactionPickerOpen  bool
	settingsOpen        bool

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

	// composerVisible toggles the bottom input row. The composer auto-hides
	// when it loses focus and reappears when the user presses 'i'.
	// composerWasFocused tracks whether focus has landed on the editor at
	// least once during the current visibility window so we don't auto-hide
	// before focus has had a chance to take effect.
	composerVisible    bool
	composerWasFocused bool
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
	state := config.LoadUIState()
	a.th.ApplyFontPrefs(prefsFromState(state.Fonts))
	a.th.ApplyThemePrefs(state.ThemeSidebar, state.ThemeMain)
	a.channels.SetFavorites(state.Favorites, a.onFavoritesChanged)
	a.channels.SetCollapsedGroups(state.CollapsedGroups, a.onCollapsedGroupsChanged)
	a.messages = newMessagesView(images)
	a.composer = newComposer()
	a.switcher = newQuickSwitcher(a.onSwitcherSelect)
	a.linkPicker = newLinkPicker(a.onLinkPickerSelect)
	a.imageViewer = newImageViewer(images)
	a.reactionPicker = newReactionPicker(a.onReactionPickerSelect)
	a.reactionPicker.SetEmojis(a.fmt.EmojiCatalog())
	a.settings = newSettingsScreen(a.th, a.onFontsChanged, a.closeSettings)

	go a.pollChannels()
	go a.pollActiveChannel()

	return a.loop()
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

func (a *App) layout(gtx layout.Context) layout.Dimensions {
	a.applyPendingFocus(gtx)
	a.handleKeys(gtx)

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
					return a.composer.Layout(gtx, a.th, placeholder, a.onSend)
				}),
			)
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

	filters := []event.Filter{
		// Global jump-to shortcut.
		key.Filter{Name: "K", Required: key.ModCtrl},
	}
	if a.switcherOpen {
		filters = append(filters,
			key.Filter{Focus: switcherEditor, Name: key.NameEscape},
			key.Filter{Focus: switcherEditor, Name: "[", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: key.NameUpArrow},
			key.Filter{Focus: switcherEditor, Name: key.NameDownArrow},
			key.Filter{Focus: switcherEditor, Name: key.NameReturn},
			key.Filter{Focus: switcherEditor, Name: "N", Required: key.ModCtrl},
			key.Filter{Focus: switcherEditor, Name: "P", Required: key.ModCtrl},
		)
	}
	if a.reactionPickerOpen {
		filters = append(filters,
			key.Filter{Focus: reactionEditor, Name: key.NameEscape},
			key.Filter{Focus: reactionEditor, Name: "[", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: key.NameUpArrow},
			key.Filter{Focus: reactionEditor, Name: key.NameDownArrow},
			key.Filter{Focus: reactionEditor, Name: key.NameReturn},
			key.Filter{Focus: reactionEditor, Name: "N", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "P", Required: key.ModCtrl},
			key.Filter{Focus: reactionEditor, Name: "W", Required: key.ModCtrl},
		)
	}
	if composerFocused {
		filters = append(filters,
			key.Filter{Focus: &a.composer.editor, Name: key.NameEscape},
			key.Filter{Focus: &a.composer.editor, Name: "[", Required: key.ModCtrl},
		)
	}
	if !composerFocused && !switcherFocused && !reactionFocused {
		// No Focus on these filters: any non-text-editing focus state (e.g. a
		// channel-row Clickable that grabbed focus on click) still routes
		// j/k/h/l/i here. Without this, clicking a row would silently disable
		// keyboard navigation.
		filters = append(filters,
			key.Filter{Name: "J"},
			key.Filter{Name: "K"},
			key.Filter{Name: "I"},
			key.Filter{Name: "H"},
			key.Filter{Name: "L"},
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
		if a.linkPickerOpen || a.imageViewerOpen {
			filters = append(filters,
				key.Filter{Name: key.NameEscape},
				key.Filter{Name: "[", Required: key.ModCtrl},
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
			case composerFocused:
				a.pendFocusKeyTag = true
				a.w.Invalidate()
			case a.messages.AuthorOpen():
				a.messages.CloseAuthor()
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
				_, msgTS, ok := a.messages.SelectedMessage()
				if ok && msgTS == ts {
					ch := a.getActiveID()
					if a.messages.InThread() {
						ch, _ = a.messages.ThreadInfo()
					}
					a.client.DeleteMessage(ch, ts)
					a.messages.SetDeletePendingTS("")
					a.w.Invalidate()
					continue
				}
			}
			a.openSelectedLinks()
		case kev.Name == "D":
			if a.focusPane == paneMessages {
				msg, ts, ok := a.messages.SelectedMessage()
				if ok && msg.UserID == a.client.GetSelfID() {
					if a.messages.DeletePendingTS() == ts {
						ch := a.getActiveID()
						if a.messages.InThread() {
							ch, _ = a.messages.ThreadInfo()
						}
						a.client.DeleteMessage(ch, ts)
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
		case a.switcherOpen && kev.Name == key.NameReturn:
			a.switcher.Submit()
		case a.reactionPickerOpen && (kev.Name == key.NameUpArrow || (kev.Name == "P" && kev.Modifiers.Contain(key.ModCtrl))):
			a.reactionPicker.MoveSelection(-1)
			a.w.Invalidate()
		case a.reactionPickerOpen && (kev.Name == key.NameDownArrow || (kev.Name == "N" && kev.Modifiers.Contain(key.ModCtrl))):
			a.reactionPicker.MoveSelection(1)
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == "W" && kev.Modifiers.Contain(key.ModCtrl):
			a.reactionPicker.DeleteLastWord()
			a.w.Invalidate()
		case a.reactionPickerOpen && kev.Name == key.NameReturn:
			a.reactionPicker.Submit()
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
			// h peels back one layer at a time: author panel → thread →
			// channels-pane focus. This keeps h/l symmetrical with the
			// l-to-drill-in progression.
			switch {
			case a.imageViewerOpen:
				a.closeImageViewer()
			case a.messages.CloseAuthor():
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
		case kev.Name == "Y":
			if a.messages.AuthorOpen() {
				if v, ok := a.messages.AuthorSelectedValue(); ok && v != "" {
					gtx.Execute(clipboard.WriteCmd{
						Type: "application/text",
						Data: io.NopCloser(strings.NewReader(v)),
					})
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
			if a.imageViewerOpen {
				a.closeImageViewer()
			} else {
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

func (a *App) onSwitcherSelect(id string) {
	a.onChannelSelect(id)
	a.messages.FocusLast()
	a.setFocusPane(paneMessages)
	a.closeSwitcher()
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
	if chID == "" {
		return
	}

	var existing []string
	for _, r := range msg.Reactions {
		if r.Name != "" {
			existing = append(existing, r.Name)
		}
	}

	a.reactionTargetCh = chID
	a.reactionTargetTS = ts
	a.reactionPicker.Reset(existing)
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
// to. Triggered by capital R.
func (a *App) echoReactions() {
	msg, ts, ok := a.messages.SelectedMessage()
	if !ok {
		return
	}
	chID := a.getActiveID()
	if chID == "" {
		return
	}
	names := make([]string, 0, len(msg.Reactions))
	for _, r := range msg.Reactions {
		if r.HasMe || r.Name == "" {
			continue
		}
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return
	}
	go func() {
		for _, name := range names {
			if err := a.client.AddReaction(chID, ts, name); err != nil {
				slog.Error("AddReaction failed", "channel", chID, "ts", ts, "emoji", name, "error", err)
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
	state := config.LoadUIState()
	state.Favorites = ids
	config.SaveUIState(state)
}

func (a *App) onCollapsedGroupsChanged(keys []string) {
	state := config.LoadUIState()
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
	state := config.LoadUIState()
	state.Fonts = stateFromTheme(a.th)
	state.ThemeSidebar = a.th.ThemeSidebar
	state.ThemeMain = a.th.ThemeMain
	config.SaveUIState(state)
	a.w.Invalidate()
}

// prefsFromState bridges config.FontPrefs → ui sectionPrefs without making
// the ui package import a runtime dependency on the JSON struct shape.
func prefsFromState(p config.FontPrefs) sectionPrefs {
	conv := func(f config.FontPref) FontStyle { return FontStyle{Face: f.Face, Size: f.Size} }
	return sectionPrefs{
		Channels: conv(p.Channels),
		Header:   conv(p.Header),
		Messages: conv(p.Messages),
		Composer: conv(p.Composer),
		Code:     conv(p.Code),
		Search:   conv(p.Search),
		UserInfo: conv(p.UserInfo),
	}
}

func stateFromTheme(th *Theme) config.FontPrefs {
	conv := func(f FontStyle) config.FontPref { return config.FontPref{Face: f.Face, Size: f.Size} }
	return config.FontPrefs{
		Channels: conv(th.Fonts.Channels),
		Header:   conv(th.Fonts.Header),
		Messages: conv(th.Fonts.Messages),
		Composer: conv(th.Fonts.Composer),
		Code:     conv(th.Fonts.Code),
		Search:   conv(th.Fonts.Search),
		UserInfo: conv(th.Fonts.UserInfo),
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
	a.activeID.Store(id)
	a.channels.SetActive(id)
	a.messages.Reset()
	if ch := a.client.Cache().GetChannel(id); ch != nil {
		a.messages.SetHeader(ch.Name, ch.Topic)
	}
	// Show whatever is already cached immediately, then trigger a fresh fetch.
	cached := a.client.Cache().GetMessages(id)
	a.messages.SetMessages(cached)
	a.w.Invalidate()
	go a.fetchMessages(id)
}

func (a *App) onSend(text string) {
	id := a.getActiveID()
	if id == "" {
		return
	}
	threadTS := ""
	if a.messages.InThread() {
		_, threadTS = a.messages.ThreadInfo()
	}
	go func() {
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
func (a *App) pollChannels() {
	// Try cached channels first for instant UI.
	if cached, err := a.client.Cache().LoadChannelsFromDisk(); err == nil && len(cached) > 0 {
		a.channels.SetChannels(cached)
		a.autoSelectFirst(cached)
		a.w.Invalidate()
	}

	tick := func() {
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

		// IM names aren't resolved by GetChannels; do it in the background so
		// the sidebar appears immediately, then refresh once names are in.
		go func(chs []slack.Channel) {
			a.client.ResolveIMNames(chs)
			a.channels.SetChannels(chs)
			a.w.Invalidate()
		}(filtered)
	}

	tick()
	t := time.NewTicker(a.cfg.Polling.ChannelList)
	defer t.Stop()
	for range t.C {
		tick()
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
		a.fetchMessages(id)
	}
}

func (a *App) fetchMessages(id string) {
	limit := a.cfg.Display.MessageLimit
	if limit <= 0 {
		limit = 50
	}
	msgs, err := a.client.GetMessages(id, limit, "")
	if err != nil {
		slog.Error("GetMessages failed", "channel", id, "error", err)
		return
	}
	if a.getActiveID() == id {
		a.messages.SetMessages(msgs)
		a.w.Invalidate()
	}
}
