package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"sync"
	"time"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"gioui.org/x/richtext"

	"github.com/user/wlslack/internal/slack"
)

// MessagesView renders the message list for the active channel.
type MessagesView struct {
	mu       sync.Mutex
	list     widget.List
	rows     []*messageRow
	header   string
	topic    string
	selected int  // index of the keyboard-highlighted row, or -1 for none
	focused  bool // true when the messages pane is the j/k target

	// pendFocusLast is set by FocusLast() when the row set is empty, so that
	// the next SetMessages call lands selection on the freshly-loaded last
	// row. The Ctrl+K switcher uses this to drop the user at the latest
	// message of the channel they just jumped to.
	pendFocusLast bool

	// pendFocusTS is set by FocusMessage() when the row set is empty.
	pendFocusTS string

	// Thread mode: when active, the pane shows the parent message plus its
	// replies instead of the channel history. j/k and selection operate on
	// the thread list while threadActive is true.
	threadActive   bool
	threadChannel  string
	threadTS       string
	threadList     widget.List
	threadRows     []*messageRow
	threadSelected int

	// Author detail panel: opened with 'l' on a selected thread message,
	// shows the author's profile fields. j/k walk the field list, 'y'
	// copies the highlighted value to the clipboard.
	authorOpen     bool
	authorRows     []authorField
	authorSelected int
	authorAvatar   string // image URL for the panel header, "" when missing
	authorName     string // display name shown next to the avatar

	deletePendingTS string

	images *slack.ImageLoader
}

type authorField struct {
	Label string
	Value string
}

type messageRow struct {
	msg     slack.Message
	rich    []richtext.InteractiveText
}

func newMessagesView(images *slack.ImageLoader) *MessagesView {
	mv := &MessagesView{images: images, selected: -1, threadSelected: -1}
	mv.list.Axis = layout.Vertical
	mv.list.ScrollToEnd = true
	mv.threadList.Axis = layout.Vertical
	mv.threadList.ScrollToEnd = true
	return mv
}

// SetFocused toggles the visual highlight on the selected row. Called by the
// app when 'l'/'h' move focus between the sidebar and the messages pane.
func (m *MessagesView) SetFocused(f bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.focused = f
}

// Reset clears selection and re-enables tail-following. Called on channel
// switch so we land at the latest message and don't carry highlight state
// across conversations.
func (m *MessagesView) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.selected = -1
	m.list.ScrollToEnd = true
	m.list.Position.BeforeEnd = false
	m.pendFocusLast = false
	m.threadActive = false
	m.threadChannel = ""
	m.threadTS = ""
	m.threadRows = nil
	m.threadSelected = -1
	m.deletePendingTS = ""
}

// FocusLast lands selection on the most recent message in the channel-history
// list. If there are no rows yet (cache miss before the API call returns),
// the request is queued so the next SetMessages applies it.
func (m *MessagesView) FocusLast() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.threadActive {
		return
	}
	if len(m.rows) == 0 {
		m.pendFocusLast = true
		return
	}
	m.selected = len(m.rows) - 1
	pos := &m.list.Position
	pos.First = m.selected
	pos.Offset = 0
	m.list.ScrollToEnd = true
}

// FocusMessage lands selection on the message with the given timestamp.
func (m *MessagesView) FocusMessage(ts string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.threadActive {
		return
	}
	if len(m.rows) == 0 {
		m.pendFocusTS = ts
		return
	}
	for i, r := range m.rows {
		if r.msg.Timestamp == ts {
			m.selected = i
			pos := &m.list.Position
			pos.First = m.selected
			pos.Offset = 0
			m.list.ScrollToEnd = false
			return
		}
	}
	m.pendFocusTS = ts
}

// PageSize returns the number of fully visible items minus one for overlap.
func (m *MessagesView) PageSize() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	pos := m.list.Position
	if m.threadActive {
		pos = m.threadList.Position
	}
	if pos.Count > 1 {
		return pos.Count - 1
	}
	return 1
}

func (m *MessagesView) CurrentMessages() []slack.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.rows
	if m.threadActive {
		rows = m.threadRows
	}
	msgs := make([]slack.Message, len(rows))
	for i, r := range rows {
		msgs[i] = r.msg
	}
	return msgs
}

// HasSelection reports whether the channel-history list (not the thread list)
// has a highlighted row that 'l' can drill into.
func (m *MessagesView) HasSelection() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.threadActive && m.selected >= 0 && m.selected < len(m.rows)
}

// SelectedMessageURLs returns the links found on whichever message currently
// has the keyboard highlight (thread row when in thread mode, otherwise the
// channel-history row). Returns nil when nothing is selected. File URLs are
// excluded — they need Slack auth and don't open in an external browser.
func (m *MessagesView) SelectedMessageURLs() []string {
	m.mu.Lock()
	msg := m.selectedMessageLocked()
	m.mu.Unlock()
	if msg == nil {
		return nil
	}
	return slack.ExtractURLs(msg.Text)
}

// SelectedMessageImages returns the image attachments of the highlighted
// message in the order Slack delivered them. Used by the in-app image viewer
// when the user presses Enter on a message that has no links.
func (m *MessagesView) SelectedMessageImages() []slack.File {
	m.mu.Lock()
	msg := m.selectedMessageLocked()
	m.mu.Unlock()
	if msg == nil {
		return nil
	}
	out := make([]slack.File, 0, len(msg.Files))
	for _, f := range msg.Files {
		if f.IsImage() {
			out = append(out, f)
		}
	}
	return out
}

// SelectedMessage returns the highlighted message (thread row when in thread
// mode, otherwise the channel-history row) plus its timestamp. ok is false
// when nothing is selected.
func (m *MessagesView) SelectedMessage() (slack.Message, string, bool) {
	m.mu.Lock()
	msg := m.selectedMessageLocked()
	m.mu.Unlock()
	if msg == nil {
		return slack.Message{}, "", false
	}
	return *msg, msg.Timestamp, true
}

func (m *MessagesView) selectedMessageLocked() *slack.Message {
	switch {
	case m.threadActive:
		if m.threadSelected >= 0 && m.threadSelected < len(m.threadRows) {
			return &m.threadRows[m.threadSelected].msg
		}
	default:
		if m.selected >= 0 && m.selected < len(m.rows) {
			return &m.rows[m.selected].msg
		}
	}
	return nil
}

// InThread reports whether the pane is currently displaying a thread.
func (m *MessagesView) InThread() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.threadActive
}

// ThreadInfo returns the channel ID and parent timestamp of the open thread.
func (m *MessagesView) ThreadInfo() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.threadChannel, m.threadTS
}

// OpenThread enters thread mode for the currently selected message and
// returns the channel/thread timestamp the caller should fetch. When the
// selected row is itself a reply, the thread root (ThreadTS) is used so we
// always render the full conversation, not just the tail.
func (m *MessagesView) OpenThread(channelID string) (string, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.threadActive || m.selected < 0 || m.selected >= len(m.rows) || channelID == "" {
		return "", "", false
	}
	r := m.rows[m.selected]
	if channelID == "__UNREADS__" && r.msg.ChannelID != "" {
		channelID = r.msg.ChannelID
	}
	ts := r.msg.ThreadTS
	if ts == "" {
		ts = r.msg.Timestamp
	}
	m.threadActive = true
	m.threadChannel = channelID
	m.threadTS = ts
	// Seed with the selected message so the parent is visible immediately,
	// before GetThreadReplies returns. SetThreadMessages will replace this
	// with the authoritative parent + reply set on the next frame.
	m.threadRows = []*messageRow{{msg: r.msg}}
	// Land highlight on the parent message so the user has an immediate
	// keyboard target. SetThreadMessages preserves this selection by ts when
	// the API result arrives.
	m.threadSelected = 0
	// Land at the top so the parent message — the one the user just drilled
	// into — is the first thing they see, with replies trailing below.
	m.threadList.ScrollToEnd = false
	m.threadList.Position = layout.Position{}
	return channelID, ts, true
}

// CloseThread exits thread mode and returns to the channel history view.
// Returns false if the pane wasn't in thread mode to begin with. Also
// dismisses the author panel since it's a child of the thread view.
func (m *MessagesView) CloseThread() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.threadActive {
		return false
	}
	m.threadActive = false
	m.threadChannel = ""
	m.threadTS = ""
	m.threadRows = nil
	m.threadSelected = -1
	m.authorOpen = false
	m.authorRows = nil
	m.authorSelected = 0
	m.deletePendingTS = ""
	return true
}

func (m *MessagesView) DeletePendingTS() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletePendingTS
}

func (m *MessagesView) SetDeletePendingTS(ts string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletePendingTS = ts
}

// HasThreadSelection reports whether the thread list has a highlighted row
// — used by the app to decide whether 'l' should drill into the author panel.
func (m *MessagesView) HasThreadSelection() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.threadActive && m.threadSelected >= 0 && m.threadSelected < len(m.threadRows)
}

// AuthorOpen reports whether the author detail panel is visible.
func (m *MessagesView) AuthorOpen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authorOpen
}

// OpenAuthor populates and shows the author panel for the currently selected
// thread message. fm is consulted for the cached profile; missing profile data
// just yields a panel with whatever IDs we have on the message itself.
func (m *MessagesView) OpenAuthor(fm *slack.Formatter) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.threadActive || m.threadSelected < 0 || m.threadSelected >= len(m.threadRows) {
		return false
	}
	r := m.threadRows[m.threadSelected]
	candidates := []authorField{{"User ID", r.msg.UserID}}

	// Add edit history if present
	for i, h := range r.msg.EditHistory {
		label := "Original"
		if i > 0 {
			label = fmt.Sprintf("Edit %d", i)
		}
		candidates = append(candidates, authorField{
			Label: label + " (" + fm.FormatTimestamp(h.Timestamp) + ")",
			Value: h.Text,
		})
	}

	avatar, displayName := "", r.msg.Username
	if u := fm.GetUser(r.msg.UserID); u != nil {
		candidates = append(candidates, []authorField{
			{"Username", u.Name},
			{"Display name", u.DisplayName},
			{"Real name", u.RealName},
			{"Title", u.Title},
			{"Email", u.Email},
			{"Phone", u.Phone},
			{"Timezone", u.Timezone},
			{"Presence", u.Presence},
			{"Status emoji", u.StatusEmoji},
			{"Status text", u.StatusText},
			{"Image URL", u.ImageURL},
		}...)
		avatar = u.ImageURL
		switch {
		case u.DisplayName != "":
			displayName = u.DisplayName
		case u.RealName != "":
			displayName = u.RealName
		case u.Name != "":
			displayName = u.Name
		}
	}
	out := candidates[:0]
	for _, f := range candidates {
		if f.Value != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		out = []authorField{{"User ID", r.msg.UserID}}
	}
	m.authorRows = out
	m.authorSelected = 0
	m.authorOpen = true
	m.authorAvatar = avatar
	m.authorName = displayName
	return true
}

// CloseAuthor hides the author panel. Returns false when it wasn't open.
func (m *MessagesView) CloseAuthor() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorOpen {
		return false
	}
	m.authorOpen = false
	m.authorRows = nil
	m.authorSelected = 0
	m.authorAvatar = ""
	m.authorName = ""
	return true
}

// MoveAuthorSelection shifts the highlighted field by delta, clamping at the
// ends. Returns false when the selection didn't change.
func (m *MessagesView) MoveAuthorSelection(delta int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorOpen || len(m.authorRows) == 0 {
		return false
	}
	idx := m.authorSelected + delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(m.authorRows) {
		idx = len(m.authorRows) - 1
	}
	if idx == m.authorSelected {
		return false
	}
	m.authorSelected = idx
	return true
}

// AuthorSelectedValue returns the value of the highlighted field, used by 'y'
// to copy to the clipboard.
func (m *MessagesView) AuthorSelectedValue() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorOpen || m.authorSelected < 0 || m.authorSelected >= len(m.authorRows) {
		return "", false
	}
	return m.authorRows[m.authorSelected].Value, true
}

// SetThreadMessages replaces the thread row set. The slack API returns the
// parent first followed by replies — we render the whole sequence so the
// reader sees the original message above the conversation.
func (m *MessagesView) SetThreadMessages(msgs []slack.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.threadActive {
		return
	}
	old := make(map[string]*messageRow, len(m.threadRows))
	for _, r := range m.threadRows {
		old[r.msg.Timestamp] = r
	}
	var selectedTS string
	if m.threadSelected >= 0 && m.threadSelected < len(m.threadRows) {
		selectedTS = m.threadRows[m.threadSelected].msg.Timestamp
	}
	out := make([]*messageRow, 0, len(msgs))
	for _, msg := range msgs {
		r, ok := old[msg.Timestamp]
		if !ok {
			r = &messageRow{}
		}
		r.msg = msg
		out = append(out, r)
	}
	m.threadRows = out
	m.threadSelected = -1
	if selectedTS != "" {
		for i, r := range out {
			if r.msg.Timestamp == selectedTS {
				m.threadSelected = i
				break
			}
		}
	}
}

// MoveSelection shifts the selection by delta and scrolls the active list to
// keep the new row visible. Returns false when there's nothing to select.
func (m *MessagesView) MoveSelection(delta int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletePendingTS = ""
	if m.threadActive {
		return moveSelectionLocked(&m.threadSelected, m.threadRows, &m.threadList, delta)
	}
	return moveSelectionLocked(&m.selected, m.rows, &m.list, delta)
}

func moveSelectionLocked(selected *int, rows []*messageRow, list *widget.List, delta int) bool {
	if len(rows) == 0 {
		return false
	}
	idx := *selected
	if idx < 0 {
		// First press from no selection lands on the most recent message —
		// the user is staring at the bottom of the chat, so put the cursor
		// where their eyes already are. From there, k walks back through
		// history.
		idx = len(rows) - 1
	} else {
		idx += delta
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	*selected = idx

	pos := &list.Position
	if pos.Count <= 0 {
		pos.First = idx
		pos.Offset = 0
	} else if idx <= pos.First {
		pos.First = idx
		pos.Offset = 0
	} else if idx >= pos.First+pos.Count-1 {
		pos.First = idx - pos.Count + 2
		if pos.First < 0 {
			pos.First = 0
		}
		if pos.First > idx {
			pos.First = idx
		}
		pos.Offset = 0
	}
	// Pin to bottom if we've selected the very last message and there are
	// actually more items than we can see.
	list.ScrollToEnd = (pos.Count < len(rows)) && idx == len(rows)-1
	return true
}

// SetHeader updates the channel name shown above the messages.
func (m *MessagesView) SetHeader(name, topic string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.header = name
	m.topic = topic
}

// SetMessages replaces the displayed messages, preserving rich-text state by
// timestamp.
func (m *MessagesView) SetMessages(msgs []slack.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := make(map[string]*messageRow, len(m.rows))
	for _, r := range m.rows {
		old[r.msg.Timestamp] = r
	}
	// Track the timestamp of the previously selected row so the highlight
	// follows the same message across refresh, even if newer messages have
	// shifted the indices.
	var selectedTS string
	if m.selected >= 0 && m.selected < len(m.rows) {
		selectedTS = m.rows[m.selected].msg.Timestamp
	}
	out := make([]*messageRow, 0, len(msgs))
	for _, msg := range msgs {
		r, ok := old[msg.Timestamp]
		if !ok {
			r = &messageRow{}
		}
		r.msg = msg
		out = append(out, r)
	}
	m.rows = out

	m.selected = -1
	if selectedTS != "" {
		for i, r := range out {
			if r.msg.Timestamp == selectedTS {
				m.selected = i
				break
			}
		}
	}
	if m.pendFocusLast && len(m.rows) > 0 {
		m.selected = len(m.rows) - 1
		pos := &m.list.Position
		pos.First = m.selected
		pos.Offset = 0
		m.list.ScrollToEnd = true
		m.pendFocusLast = false
	}
	if m.pendFocusTS != "" && len(m.rows) > 0 {
		for i, r := range m.rows {
			if r.msg.Timestamp == m.pendFocusTS {
				m.selected = i
				pos := &m.list.Position
				pos.First = m.selected
				pos.Offset = 0
				m.list.ScrollToEnd = false
				break
			}
		}
		m.pendFocusTS = ""
	}
}

// Layout draws the header bar plus the scrollable list. Switches between
// the channel history and thread reply list based on threadActive. When the
// author panel is open, it's rendered as a fixed-width side frame to the
// right of the message list.
func (m *MessagesView) Layout(gtx layout.Context, th *Theme, fmt *slack.Formatter) layout.Dimensions {
	m.mu.Lock()
	authorOpen := m.authorOpen
	m.mu.Unlock()

	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return m.layoutMessages(gtx, th, fmt)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !authorOpen {
				return layout.Dimensions{}
			}
			gtx.Constraints.Min.X = gtx.Dp(unit.Dp(320))
			gtx.Constraints.Max.X = gtx.Dp(unit.Dp(320))
			return m.layoutAuthor(gtx, th)
		}),
	)
}

func (m *MessagesView) layoutMessages(gtx layout.Context, th *Theme, fmt *slack.Formatter) layout.Dimensions {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := &m.list
	rows := m.rows
	if m.threadActive {
		list = &m.threadList
		rows = m.threadRows
	}
	n := len(rows)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.layoutHeaderLocked(gtx, th)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
				return material.List(th.Mat, list).Layout(gtx, n, func(gtx layout.Context, idx int) layout.Dimensions {
					if idx >= len(rows) {
						return layout.Dimensions{}
					}
					return m.layoutRowLocked(gtx, th, fmt, idx, rows)
				})
			})
		}),
	)
}

func (m *MessagesView) layoutAuthor(gtx layout.Context, th *Theme) layout.Dimensions {
	m.mu.Lock()
	defer m.mu.Unlock()

	return withBorder(gtx, th.Pal.Border, borders{Left: true}, func(gtx layout.Context) layout.Dimensions {
	return paintedBg(gtx, th.Pal.BgSidebar, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(14),
			Bottom: unit.Dp(14),
			Left:   unit.Dp(16),
			Right:  unit.Dp(16),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			children := make([]layout.FlexChild, 0, len(m.authorRows)+5)
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return m.layoutAuthorHeaderLocked(gtx, th)
			}))
			children = append(children, layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout))
			for i, f := range m.authorRows {
				i, f := i, f
				children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return m.layoutAuthorFieldLocked(gtx, th, i, f)
				}))
			}
			children = append(children, layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout))
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th.Mat, "j/k navigate · y copy · h close")
				th.applyFont(&lbl, FontStyle{})
				lbl.Color = th.Pal.TextMuted
				return lbl.Layout(gtx)
			}))
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
		})
	})
	})
}

// layoutAuthorHeaderLocked renders the avatar (when known) plus the user's display
// name above the field list. The avatar is requested from the same async
// loader used for inline message images, so it pops in after the first frame.
func (m *MessagesView) layoutAuthorHeaderLocked(gtx layout.Context, th *Theme) layout.Dimensions {
	const avatarDp = 96
	avatarOp, hasAvatar := m.authorAvatarOpLocked(gtx)
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			if !hasAvatar {
				return layout.Dimensions{}
			}
			gtx.Constraints.Max.X = gtx.Dp(unit.Dp(avatarDp))
			gtx.Constraints.Max.Y = gtx.Dp(unit.Dp(avatarDp))
			gtx.Constraints.Min = gtx.Constraints.Max
			w := widget.Image{
				Src:      avatarOp,
				Fit:      widget.Cover,
				Position: layout.Center,
			}
			return w.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			name := m.authorName
			if name == "" {
				name = "Author"
			}
			lbl := material.Subtitle1(th.Mat, name)
			lbl.Color = th.Pal.Text
			lbl.Font.Weight = font.Bold
			lbl.Alignment = text.Middle
			th.applyFont(&lbl, th.Fonts.UserInfo)
			return lbl.Layout(gtx)
		}),
	)
}

// authorAvatarOpLocked returns the cached image op for the panel avatar.
func (m *MessagesView) authorAvatarOpLocked(_ layout.Context) (paint.ImageOp, bool) {
	if m.authorAvatar == "" || m.images == nil {
		return paint.ImageOp{}, false
	}
	op, hasOp, _ := m.images.GetOp(m.authorAvatar)
	return op, hasOp
}

func (m *MessagesView) layoutAuthorFieldLocked(gtx layout.Context, th *Theme, idx int, f authorField) layout.Dimensions {
	bg := th.Pal.BgSidebar
	if idx == m.authorSelected {
		bg = WithAlpha(th.Pal.Selection, 0x40)
	}
	return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(4),
			Bottom: unit.Dp(4),
			Left:   unit.Dp(6),
			Right:  unit.Dp(6),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Caption(th.Mat, f.Label)
					lbl.Color = th.Pal.TextDim
					th.applyFont(&lbl, th.Fonts.UserInfo)
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, f.Value)
					lbl.Color = th.Pal.Text
					th.applyFont(&lbl, th.Fonts.UserInfo)
					return lbl.Layout(gtx)
				}),
			)
		})
	})
}

func (m *MessagesView) layoutHeaderLocked(gtx layout.Context, th *Theme) layout.Dimensions {
	if m.header == "" {
		return layout.Dimensions{Size: gtx.Constraints.Min}
	}
	title := "# " + m.header
	subtitle := m.topic
	if m.threadActive {
		title = "↳ Thread in #" + m.header
		subtitle = "press h to return to #" + m.header
	}
	return withBorder(gtx, th.Pal.Border, borders{Bottom: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.Pal.BgHeader, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(10),
				Bottom: unit.Dp(10),
				Left:   unit.Dp(16),
				Right:  unit.Dp(16),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Subtitle1(th.Mat, title)
						lbl.Color = th.Pal.TextStrong
						lbl.Font.Weight = font.SemiBold
						th.applyFont(&lbl, th.Fonts.Header)
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if subtitle == "" {
							return layout.Dimensions{}
						}
						return layout.Inset{Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Caption(th.Mat, subtitle)
							lbl.Color = th.Pal.TextDim
							th.applyFont(&lbl, th.Fonts.Header)
							return lbl.Layout(gtx)
						})
					}),
				)
			})
		})
	})
}

func dayChanged(ts1, ts2 string) bool {
	t1, ok1 := slack.ParseTimestamp(ts1)
	t2, ok2 := slack.ParseTimestamp(ts2)
	if !ok1 || !ok2 {
		return false
	}
	y1, m1, d1 := t1.Date()
	y2, m2, d2 := t2.Date()
	return y1 != y2 || m1 != m2 || d1 != d2
}

func (m *MessagesView) layoutDayDivider(gtx layout.Context, th *Theme, ts string) layout.Dimensions {
	return layout.Inset{
		Top:    unit.Dp(16),
		Bottom: unit.Dp(8),
		Left:   unit.Dp(16),
		Right:  unit.Dp(16),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				sz := image.Point{X: gtx.Constraints.Max.X, Y: gtx.Dp(unit.Dp(1))}
				drawRect(gtx, th.Pal.Border, sz)
				return layout.Dimensions{Size: sz}
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					t, ok := slack.ParseTimestamp(ts)
					if !ok {
						return layout.Dimensions{}
					}

					var dateStr string
					now := time.Now()
					y, m, d := t.Date()
					ny, nm, nd := now.Date()

					if y == ny && m == nm && d == nd {
						dateStr = "Today"
					} else {
						yesterday := now.AddDate(0, 0, -1)
						yy, ym, yd := yesterday.Date()
						if y == yy && m == ym && d == yd {
							dateStr = "Yesterday"
						} else {
							dateStr = t.Format("Monday, January 2")
							if y != ny {
								dateStr = t.Format("Monday, January 2, 2006")
							}
						}
					}

					lbl := material.Caption(th.Mat, dateStr)
					lbl.Color = th.Pal.TextMuted
					lbl.Font.Weight = font.Bold
					th.applyFont(&lbl, FontStyle{})
					return lbl.Layout(gtx)
				})
			}),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				sz := image.Point{X: gtx.Constraints.Max.X, Y: gtx.Dp(unit.Dp(1))}
				drawRect(gtx, th.Pal.Border, sz)
				return layout.Dimensions{Size: sz}
			}),
		)
	})
}

func (m *MessagesView) layoutRowLocked(gtx layout.Context, th *Theme, fmt *slack.Formatter, idx int, rows []*messageRow) layout.Dimensions {
	r := rows[idx]
	selected := m.selected
	if m.threadActive {
		selected = m.threadSelected
	}
	isSelected := m.focused && idx == selected
	bg := th.Pal.Bg
	if isSelected {
		bg = th.Pal.BgRowAlt
	}
	showDivider := idx == 0
	if idx > 0 {
		showDivider = dayChanged(rows[idx-1].msg.Timestamp, r.msg.Timestamp)
	}

	row := func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(8),
				Bottom: unit.Dp(8),
				Left:   unit.Dp(16),
				Right:  unit.Dp(16),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx,
					// Left column: Avatar
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						var op paint.ImageOp
						var hasOp bool
						u := fmt.GetUser(r.msg.UserID)
						if u != nil && u.ImageURL != "" {
							op, hasOp, _ = m.images.GetOp(u.ImageURL)
						}
						return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if !hasOp {
									return layout.Spacer{Width: unit.Dp(36), Height: unit.Dp(36)}.Layout(gtx)
								}
								gtx.Constraints.Max.X = gtx.Dp(unit.Dp(36))
								gtx.Constraints.Max.Y = gtx.Dp(unit.Dp(36))
								gtx.Constraints.Min = gtx.Constraints.Max
								w := widget.Image{
									Src:      op,
									Fit:      widget.Cover,
									Position: layout.Center,
								}
								return w.Layout(gtx)
							}),
							layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						)
					}),
					// Right column: Content
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							// Header line: username + time
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										name := r.msg.Username
										if name == "" {
											name = r.msg.UserID
										}
										var presence string
										if u := fmt.GetUser(r.msg.UserID); u != nil {
											presence = u.Presence
										}

										return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												if presence == "" {
													return layout.Dimensions{}
												}
												dot := "○ "
												col := th.Pal.PresenceAway
												if presence == "active" {
													dot = "● "
													col = th.Pal.PresenceActive
												}
												lbl := material.Body1(th.Mat, dot)
												lbl.Color = col
												th.applyFont(&lbl, th.Fonts.Messages)
												return lbl.Layout(gtx)
											}),
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												lbl := material.Body1(th.Mat, name)
												lbl.Font.Weight = font.SemiBold
												lbl.Color = th.Pal.TextStrong
												th.applyFont(&lbl, th.Fonts.Messages)
												return lbl.Layout(gtx)
											}),
										)
									}),
									layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										lbl := material.Caption(th.Mat, fmt.FormatTimestamp(r.msg.Timestamp))
										lbl.Color = th.Pal.TextMuted
										th.applyFont(&lbl, th.Fonts.Messages)
										return lbl.Layout(gtx)
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										if !r.msg.Edited && !r.msg.Deleted {
											return layout.Dimensions{}
										}
										text := ""
										color := th.Pal.TextMuted
										if r.msg.Deleted {
											text = " [DELETED]"
											color = th.Pal.Firing
										} else if r.msg.Edited {
											text = " (edited)"
										}
										return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
											lbl := material.Caption(th.Mat, text)
											lbl.Color = color
											lbl.Font.Style = font.Italic
											th.applyFont(&lbl, th.Fonts.Messages)
											return lbl.Layout(gtx)
										})
									}),
									layout.Rigid(func(gtx layout.Context) layout.Dimensions {
										if r.msg.ChannelName == "" {
											return layout.Dimensions{}
										}
										return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
											layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												lbl := material.Caption(th.Mat, "#"+r.msg.ChannelName)
												lbl.Color = th.Pal.TextMuted
												lbl.Font.Style = font.Italic
												th.applyFont(&lbl, th.Fonts.Messages)
												return lbl.Layout(gtx)
											}),
										)
									}),
								)
							}),
							// Body: styled richtext
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return m.layoutBody(gtx, th, fmt, r)
							}),
							// Inline image previews
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								return m.layoutFiles(gtx, th, r.msg.Files)
							}),
							// Reactions row (compact)
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if len(r.msg.Reactions) == 0 {
									return layout.Dimensions{}
								}
								return layout.Inset{Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
									return m.layoutReactions(gtx, th, fmt, r.msg.Reactions)
								})
							}),
							// Thread indicator (only on the channel-history view; inside a
							// thread every row is by definition part of one).
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if m.threadActive || r.msg.ReplyCount <= 0 {
									return layout.Dimensions{}
								}
								return m.layoutThreadBadge(gtx, th, fmt, &r.msg)
							}),
							// Deletion pending prompt
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if m.deletePendingTS == r.msg.Timestamp {
									return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										lbl := material.Body2(th.Mat, "Press 'd' or 'Enter' again to delete...")
										lbl.Color = th.Pal.Firing
										lbl.Font.Weight = font.SemiBold
										return lbl.Layout(gtx)
									})
								}
								return layout.Dimensions{}
							}),
						)
					}),
				)
			})
		})
	}

	var out layout.Widget
	if isSelected {
		out = func(gtx layout.Context) layout.Dimensions {
			return withBorder(gtx, th.Pal.Accent, borders{Left: true}, row)
		}
	} else {
		out = row
	}

	if !showDivider {
		return out(gtx)
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.layoutDayDivider(gtx, th, r.msg.Timestamp)
		}),
		layout.Rigid(out),
	)
}

func (m *MessagesView) layoutBody(gtx layout.Context, th *Theme, fm *slack.Formatter, r *messageRow) layout.Dimensions {
	spans := fm.FormatSpans(r.msg.Text)
	if len(spans) == 0 {
		return layout.Dimensions{}
	}

	type chunk struct {
		isCodeBlock bool
		spans       []slack.Span
	}
	var chunks []chunk
	for _, s := range spans {
		isCB := s.Style&slack.StyleCodeBlock != 0
		if len(chunks) == 0 || chunks[len(chunks)-1].isCodeBlock != isCB {
			chunks = append(chunks, chunk{isCodeBlock: isCB, spans: []slack.Span{s}})
		} else {
			chunks[len(chunks)-1].spans = append(chunks[len(chunks)-1].spans, s)
		}
	}

	if len(r.rich) < len(chunks) {
		r.rich = make([]richtext.InteractiveText, len(chunks))
	}

	var children []layout.FlexChild
	for i, c := range chunks {
		i := i
		c := c
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			styles := make([]richtext.SpanStyle, 0, len(c.spans))
			for _, s := range c.spans {
				span := toRichSpan(s, th)
				if r.msg.Deleted {
					span.Color = th.Pal.TextMuted
					// Gio doesn't have strikethrough in richtext.SpanStyle yet,
					// but we can dim it even more.
					span.Color.A = 0x80
				}
				styles = append(styles, span)
			}
			w := richtext.Text(&r.rich[i], th.Mat.Shaper, styles...)
			if c.isCodeBlock {
				return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return paintedBg(gtx, th.Pal.BgCode, func(gtx layout.Context) layout.Dimensions {
						return withBorder(gtx, th.Pal.Border, borders{Top: true, Bottom: true, Left: true, Right: true}, func(gtx layout.Context) layout.Dimensions {
							return layout.Inset{Top: unit.Dp(8), Bottom: unit.Dp(8), Left: unit.Dp(8), Right: unit.Dp(8)}.Layout(gtx, w.Layout)
						})
					})
				})
			}
			return w.Layout(gtx)
		}))
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// layoutFiles renders inline previews for image attachments. Non-image files
// fall through as a small "name (mimetype)" caption so they're at least
// discoverable; future work can extend this for other types.
func (m *MessagesView) layoutFiles(gtx layout.Context, th *Theme, files []slack.File) layout.Dimensions {
	if len(files) == 0 || m.images == nil {
		return layout.Dimensions{}
	}
	const maxW, maxH = 200, 200 // dp; thumbnail-sized preview cap
	children := make([]layout.FlexChild, 0, len(files)*2)
	first := true
	for _, f := range files {
		f := f
		if !f.IsImage() {
			if !first {
				children = append(children, layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout))
			}
			first = false
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th.Mat, "📎 "+f.Name)
				lbl.Color = th.Pal.TextDim
				return lbl.Layout(gtx)
			}))
			continue
		}
		if !first {
			children = append(children, layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout))
		}
		first = false
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.layoutImage(gtx, th, f, maxW, maxH)
		}))
	}
	return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Start}.Layout(gtx, children...)
	})
}

func (m *MessagesView) layoutImage(gtx layout.Context, th *Theme, f slack.File, maxW, maxH int) layout.Dimensions {
	url := f.ThumbnailURL()
	if f.Mimetype == "image/gif" && f.URL != "" {
		url = f.URL
	}

	if g, done := m.images.GetGif(url); done && g != nil && len(g.Image) > 1 {
		return m.layoutGif(gtx, th, g, maxW, maxH)
	}

	op, hasOp, done := m.images.GetOp(url)
	if !done {
		lbl := material.Caption(th.Mat, "🖼  loading "+f.Name+"…")
		lbl.Color = th.Pal.TextDim
		return lbl.Layout(gtx)
	}
	if !hasOp {
		lbl := material.Caption(th.Mat, "🖼  "+f.Name+" (failed to load)")
		lbl.Color = th.Pal.TextDim
		return lbl.Layout(gtx)
	}
	// Make all thumbnails a uniform square size.
	// We use the minimum of maxPxW and maxPxH to ensure it's a perfect square
	// that respects both the constraints and the provided max dimensions.
	sz := op.Size()
	if sz.X <= 0 || sz.Y <= 0 {
		return layout.Dimensions{}
	}
	maxPxW := min(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(maxW)))
	maxPxH := gtx.Dp(unit.Dp(maxH))
	if maxPxW <= 0 || maxPxH <= 0 {
		return layout.Dimensions{}
	}
	
	sizePx := min(maxPxW, maxPxH)
	target := image.Point{
		X: sizePx,
		Y: sizePx,
	}
	
	if target.X < 1 || target.Y < 1 {
		return layout.Dimensions{}
	}
	gtx.Constraints = layout.Exact(target)
	w := widget.Image{
		Src:      op,
		Fit:      widget.Cover,
		Position: layout.Center,
	}
	return w.Layout(gtx)
}

func (m *MessagesView) layoutGif(gtx layout.Context, th *Theme, g *gif.GIF, maxW, maxH int) layout.Dimensions {
	var totalDuration time.Duration
	for _, d := range g.Delay {
		totalDuration += time.Duration(d) * 10 * time.Millisecond
	}
	if totalDuration <= 0 {
		totalDuration = 100 * time.Millisecond
	}

	rem := gtx.Now.Sub(time.Time{}) % totalDuration
	var currentFrame int
	var frameStart time.Duration
	for i, d := range g.Delay {
		dura := time.Duration(d) * 10 * time.Millisecond
		if rem < frameStart+dura {
			currentFrame = i
			break
		}
		frameStart += dura
	}

	img := g.Image[currentFrame]
	imgOp := paint.NewImageOp(img)

	maxPxW := min(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(maxW)))
	maxPxH := gtx.Dp(unit.Dp(maxH))
	sizePx := min(maxPxW, maxPxH)
	target := image.Point{X: sizePx, Y: sizePx}
	if target.X < 1 || target.Y < 1 {
		return layout.Dimensions{}
	}

	gtx.Constraints = layout.Exact(target)

	// Schedule next frame
	nextFrameDelay := time.Duration(g.Delay[currentFrame]) * 10 * time.Millisecond
	nextFrameAt := gtx.Now.Add(frameStart + nextFrameDelay - rem)
	gtx.Execute(op.InvalidateCmd{At: nextFrameAt})

	w := widget.Image{
		Src:      imgOp,
		Fit:      widget.Cover,
		Position: layout.Center,
	}
	return w.Layout(gtx)
}

// layoutThreadBadge renders a caption with participant avatars, reply count,
// and last reply age pinned under the message body.
func (m *MessagesView) layoutThreadBadge(gtx layout.Context, th *Theme, fm *slack.Formatter, msg *slack.Message) layout.Dimensions {
	noun := "replies"
	if msg.ReplyCount == 1 {
		noun = "reply"
	}

	age := ""
	if msg.LastReplyTS != "" {
		age = fm.FormatTimestampAge(msg.LastReplyTS)
	}

	return layout.Inset{Top: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if len(msg.ReplyUsers) == 0 {
					return layout.Dimensions{}
				}

				var children []layout.FlexChild
				for i, userID := range msg.ReplyUsers {
					if i >= 5 {
						break
					}
					userID := userID
					children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						u := fm.GetUser(userID)
						var op paint.ImageOp
						var hasOp bool
						if u != nil && u.ImageURL != "" {
							op, hasOp, _ = m.images.GetOp(u.ImageURL)
						}

						size := unit.Dp(16)
						return layout.Inset{Right: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							if !hasOp {
								return layout.Spacer{Width: size, Height: size}.Layout(gtx)
							}
							gtx.Constraints.Max = image.Pt(gtx.Dp(size), gtx.Dp(size))
							gtx.Constraints.Min = gtx.Constraints.Max
							w := widget.Image{
								Src:      op,
								Fit:      widget.Cover,
								Position: layout.Center,
							}
							return w.Layout(gtx)
						})
					}))
				}
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
			}),
			layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				text := fmt.Sprintf("%d %s", msg.ReplyCount, noun)
				if age != "" {
					text += " • last " + age
				}
				lbl := material.Body2(th.Mat, text)
				lbl.Color = th.Pal.Accent
				lbl.Font.Weight = font.Bold
				th.applyFont(&lbl, th.Fonts.Threads)
				return lbl.Layout(gtx)
			}),
		)
	})
}

func (m *MessagesView) layoutReactions(gtx layout.Context, th *Theme, fm *slack.Formatter, rs []slack.Reaction) layout.Dimensions {
	children := make([]layout.FlexChild, 0, len(rs)*2)
	for _, r := range rs {
		r := r
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.layoutReactionChip(gtx, th, fm, r)
		}))
		children = append(children, layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout))
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx, children...)
}

func (m *MessagesView) layoutReactionChip(gtx layout.Context, th *Theme, fm *slack.Formatter, r slack.Reaction) layout.Dimensions {
	bg := th.Pal.BgCode
	textColor := th.Pal.Text
	borderColor := th.Pal.Border
	if r.HasMe {
		bg = WithAlpha(th.Pal.Accent, 0x33)
		textColor = th.Pal.TextStrong
		borderColor = WithAlpha(th.Pal.Accent, 0x88)
	}
	return withBorder(gtx, borderColor, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
	return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(2),
			Bottom: unit.Dp(2),
			Left:   unit.Dp(6),
			Right:  unit.Dp(6),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if fm.IsCustomEmoji(r.Name) {
						url := fm.FormatEmoji(r.Name)
						imgOp, ok, done := m.images.GetOp(url)
						if !done {
							// Loading
							gtx.Constraints.Min = image.Point{X: gtx.Dp(18), Y: gtx.Dp(18)}
							return layout.Dimensions{Size: gtx.Constraints.Min}
						}
						if ok {
							target := gtx.Dp(18)
							size := imgOp.Size()
							scale := float32(target) / float32(max(size.X, size.Y))
							dx := (float32(target) - float32(size.X)*scale) / 2
							dy := (float32(target) - float32(size.Y)*scale) / 2

							stack := op.Affine(f32.Affine2D{}.Scale(f32.Pt(0, 0), f32.Pt(scale, scale)).Offset(f32.Pt(dx/scale, dy/scale))).Push(gtx.Ops)
							imgOp.Add(gtx.Ops)
							paint.PaintOp{}.Add(gtx.Ops)
							stack.Pop()
							return layout.Dimensions{Size: image.Point{X: target, Y: target}}
						}
					}
					emoji := material.Body2(th.Mat, fm.FormatEmoji(r.Name))
					emoji.Color = textColor
					return emoji.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Width: unit.Dp(4)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					count := r.Count
					if count <= 0 {
						count = 1
					}
					lbl := material.Body2(th.Mat, fmt.Sprintf("%d", count))
					lbl.Color = textColor
					lbl.Font.Weight = font.Medium
					return lbl.Layout(gtx)
				}),
			)
		})
	})
	})
}

// toRichSpan converts a backend Span to a Gio richtext SpanStyle, applying
// the current theme's palette and font choices.
func toRichSpan(s slack.Span, th *Theme) richtext.SpanStyle {
	bodyFont, bodySize := th.FontFor(th.Fonts.Messages)
	out := richtext.SpanStyle{
		Content: s.Text,
		Color:   th.Pal.Text,
		Size:    bodySize,
		Font:    bodyFont,
	}
	if s.Style&slack.StyleBold != 0 {
		out.Font.Weight = font.Bold
	}
	if s.Style&slack.StyleItalic != 0 {
		out.Font.Style = font.Italic
	}
	if s.Style&slack.StyleStrike != 0 {
		// Gio doesn't render strikethrough natively; dim the color so the
		// reader sees something different from regular text.
		out.Color = th.Pal.TextDim
	}
	if s.Style&(slack.StyleCode|slack.StyleCodeBlock) != 0 {
		codeFace := th.Fonts.Code.Face
		if codeFace == "" {
			codeFace = string(th.MonoF.Typeface)
		}
		out.Font.Typeface = font.Typeface(codeFace)
		if th.Fonts.Code.Size > 0 {
			out.Size = unit.Sp(th.Fonts.Code.Size)
		}
		out.Color = lighten(th.Pal.Text)
	}
	if s.Style&slack.StyleLink != 0 {
		out.Color = th.Pal.Link
		out.Interactive = true
		if s.Link != "" {
			out.Set("url", s.Link)
		}
	}
	switch {
	case s.Style&slack.StyleStaging != 0:
		out.Color = th.Pal.Staging
	case s.Style&slack.StyleProduction != 0:
		out.Color = th.Pal.Production
	case s.Style&slack.StyleResolved != 0:
		out.Color = th.Pal.Resolved
		out.Content = "● " + out.Content
	case s.Style&slack.StyleFiring != 0:
		out.Color = th.Pal.Firing
		out.Content = "● " + out.Content
	case s.Style&slack.StyleMention != 0:
		out.Color = th.Pal.Mention
		out.Font.Weight = font.Bold
	case s.Style&slack.StyleChannel != 0:
		out.Color = th.Pal.Link
		out.Font.Weight = font.Bold
	}
	return out
}

func lighten(c color.NRGBA) color.NRGBA {
	add := func(v uint8, by uint8) uint8 {
		if int(v)+int(by) > 255 {
			return 255
		}
		return v + by
	}
	return color.NRGBA{R: add(c.R, 16), G: add(c.G, 16), B: add(c.B, 16), A: c.A}
}
