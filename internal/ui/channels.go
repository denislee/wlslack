package ui

import (
	"fmt"
	"image/color"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// ChannelsSidebar renders the left-hand list of channels.
type ChannelsSidebar struct {
	mu       sync.Mutex
	dirty    bool
	list     widget.List
	rows     []*sidebarRow
	activeID string
	onSelect func(id string)

	// cursorKey marks the keyboard-navigation cursor row. It can land on
	// either a channel (key == channel ID) or a group header (key ==
	// headerKey(name)), so j/k can step through headers and let space/enter
	// collapse them. activeID still tracks which channel's messages are
	// shown -- the two diverge whenever the cursor sits on a header.
	cursorKey string

	// favorites is the set of channel IDs the user has starred. Updates flow
	// through the host App via onFavoritesChanged so the new set can be
	// persisted to disk.
	favorites          map[string]bool
	onFavoritesChanged func([]string)

	// collapsed is the set of group header keys whose children are hidden.
	// onCollapsedChanged fires on toggle so the host can persist the state.
	collapsed          map[string]bool
	onCollapsedChanged func([]string)

	// hidden is the set of channel IDs the user has chosen to hide via config.
	// They normally don't appear in the sidebar; when they do (because of
	// unreads/mentions) they are floated to the top of their category so the
	// activity isn't lost behind never-hidden channels.
	hidden map[string]bool

	// raw retains the most recent unsorted channel set so we can re-group when
	// favorites change without waiting for the next poll.
	raw []slack.Channel

	showOnlyRecent bool
	hideEmpty      bool
}

type rowKind int

const (
	rowChannel rowKind = iota
	rowHeader
)

type sidebarRow struct {
	kind       rowKind
	header     string
	headerKey  string
	collapsed  bool
	click      widget.Clickable
	channel    slack.Channel
	isFavorite bool
}

func newChannelsSidebar(onSelect func(id string)) *ChannelsSidebar {
	cs := &ChannelsSidebar{
		onSelect:  onSelect,
		favorites: make(map[string]bool),
		collapsed: make(map[string]bool),
		hidden:    make(map[string]bool),
	}
	cs.list.Axis = layout.Vertical
	return cs
}

// SetHidden replaces the set of channel IDs flagged hidden in config. Hidden
// channels are filtered upstream when they have no unreads; the sidebar uses
// this set to float the ones that did surface to the top of their category.
func (s *ChannelsSidebar) SetHidden(ids []string) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hidden = set
	if s.raw != nil {
		s.dirty = true
	}
}

// SetActive marks the given channel ID as the highlighted one. The cursor
// follows the active channel so that subsequent j/k keeps stepping from
// where the user is looking.
func (s *ChannelsSidebar) SetActive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeID = id
	s.cursorKey = id
}

// SetFavorites replaces the favorites set and registers a change callback.
// The callback fires whenever ToggleFavoriteOnActive flips a channel so the
// host can persist the new list.
func (s *ChannelsSidebar) SetFavorites(ids []string, onChanged func([]string)) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.favorites = set
	s.onFavoritesChanged = onChanged
	if s.raw != nil {
		s.dirty = true
	}
}

// SetCollapsedGroups replaces the collapsed-group set and registers a change
// callback. The callback fires whenever a header is clicked so the host can
// persist the new list.
func (s *ChannelsSidebar) SetCollapsedGroups(keys []string, onChanged func([]string)) {
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collapsed = set
	s.onCollapsedChanged = onChanged
	if s.raw != nil {
		s.dirty = true
	}
}

func (s *ChannelsSidebar) collapsedSlice() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collapsedSliceLocked()
}

func (s *ChannelsSidebar) collapsedSliceLocked() []string {
	out := make([]string, 0, len(s.collapsed))
	for k, v := range s.collapsed {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// ToggleFavoriteOnActive flips the favorite state of the highlighted channel.
// Returns true if a change was made.
func (s *ChannelsSidebar) ToggleFavoriteOnActive() bool {
	s.mu.Lock()
	if s.activeID == "" {
		s.mu.Unlock()
		return false
	}
	if s.favorites[s.activeID] {
		delete(s.favorites, s.activeID)
	} else {
		s.favorites[s.activeID] = true
	}
	s.rebuildRowsLocked()
	onFavoritesChanged := s.onFavoritesChanged
	favs := s.favoritesSliceLocked()
	s.mu.Unlock()

	if onFavoritesChanged != nil {
		onFavoritesChanged(favs)
	}
	return true
}

func (s *ChannelsSidebar) favoritesSliceLocked() []string {
	out := make([]string, 0, len(s.favorites))
	for id := range s.favorites {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// FirstID returns the ID of the first selectable (non-header) row, or "" if
// the list has no channel rows yet.
func (s *ChannelsSidebar) FirstID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.kind == rowChannel {
			return r.channel.ID
		}
	}
	return ""
}

// PageSize returns the number of fully visible items minus one for overlap.
func (s *ChannelsSidebar) PageSize() int {
	// list.Position is not protected by mutex as it should only be touched by UI thread.
	if s.list.Position.Count > 1 {
		return s.list.Position.Count - 1
	}
	return 1
}

// rowKey returns the cursor key for r -- channel ID for channel rows,
// headerKey(name) for header rows. Used to track keyboard cursor position
// stably across rebuilds.
func rowKeyFor(r *sidebarRow) string {
	if r.kind == rowHeader {
		return headerKey(r.headerKey)
	}
	return r.channel.ID
}

// MoveSelection shifts the cursor by delta (positive = down) over both
// channel and header rows. The list is scrolled so the new row stays
// visible. Returns ("", false) when there are no rows yet, ("", true) when
// the cursor lands on a header, and (channelID, true) when it lands on a
// channel -- callers should only switch the active channel in the third case.
func (s *ChannelsSidebar) MoveSelection(delta int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) == 0 {
		return "", false
	}

	cur := -1
	if s.cursorKey != "" {
		for i, r := range s.rows {
			if rowKeyFor(r) == s.cursorKey {
				cur = i
				break
			}
		}
	}
	if cur < 0 && s.activeID != "" {
		for i, r := range s.rows {
			if r.kind == rowChannel && r.channel.ID == s.activeID {
				cur = i
				break
			}
		}
	}
	if cur < 0 {
		if delta < 0 {
			cur = len(s.rows) - 1
		} else {
			cur = 0
		}
	} else {
		cur += delta
	}
	if cur < 0 {
		cur = 0
	}
	if cur >= len(s.rows) {
		cur = len(s.rows) - 1
	}
	r := s.rows[cur]
	s.cursorKey = rowKeyFor(r)

	// Keep the selection inside the visible window. Position.Count is the
	// number of items the list rendered last frame; before the first layout
	// it's 0, so just snap First to the new index in that case.
	pos := &s.list.Position
	if pos.Count <= 0 {
		pos.First = cur
		pos.Offset = 0
	} else if cur < pos.First {
		pos.First = cur
		pos.Offset = 0
	} else if cur >= pos.First+pos.Count {
		pos.First = cur - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
	if r.kind == rowChannel {
		s.activeID = r.channel.ID
		return r.channel.ID, true
	}
	return "", true
}

// ToggleCursorHeader flips the collapsed state of the header row under the
// cursor, if any. Returns true if a toggle happened so the caller can
// invalidate the window.
func (s *ChannelsSidebar) ToggleCursorHeader() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursorKey == "" {
		return false
	}
	for _, r := range s.rows {
		if r.kind != rowHeader {
			continue
		}
		if headerKey(r.headerKey) != s.cursorKey {
			continue
		}
		s.collapsed[r.headerKey] = !s.collapsed[r.headerKey]
		s.rebuildRowsLocked()
		if s.onCollapsedChanged != nil {
			onCollapsedChanged := s.onCollapsedChanged
			collapsed := s.collapsedSliceLocked()
			s.mu.Unlock()
			onCollapsedChanged(collapsed)
			s.mu.Lock()
		}
		return true
	}
	return false
}

// SetChannels rebuilds the row list from the latest channel snapshot.
func (s *ChannelsSidebar) SetChannels(channels []slack.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = channels
	s.dirty = true
}

// rebuildRows splits channels into Favorites plus the four conversation-type
// groups (channels, external, DMs, group DMs) and sorts each. Click state for
// previously rendered channel rows is reused so highlights survive the rebuild.
func (s *ChannelsSidebar) rebuildRowsLocked() {
	old := make(map[string]*sidebarRow, len(s.rows))
	for _, r := range s.rows {
		switch r.kind {
		case rowChannel:
			old[r.channel.ID] = r
		case rowHeader:
			old[headerKey(r.headerKey)] = r
		}
	}

	// Unread aggregates channels with unread messages from all conversation
	// types so the user can scan them in one place. A channel only ever lives
	// in one group -- being unread takes priority over its category.
	var unread, favs, channels, externals, dms, mpdms []slack.Channel
	hasAnyUnread := false
	for _, ch := range s.raw {
		isFav := s.favorites[ch.ID]
		if s.hideEmpty && ch.LatestTSVerified && ch.LatestTS == "" && ch.UnreadCount == 0 && !isFav && !ch.IsIM && !ch.IsMPIM {
			slog.Debug("sidebar: filtering verified empty channel", "id", ch.ID, "name", ch.Name, "unread", ch.UnreadCount, "latest", ch.LatestTS)
			continue
		}
		if s.hideEmpty && !isFav && !ch.IsIM && !ch.IsMPIM {
			slog.Debug("sidebar: keeping channel", "id", ch.ID, "name", ch.Name, "unread", ch.UnreadCount, "latest", ch.LatestTS, "verified", ch.LatestTSVerified)
		}

		if ch.MentionCount > 0 {
			hasAnyUnread = true
		}

		switch {
		case isFav:
			favs = append(favs, ch)
		case ch.IsIM:
			dms = append(dms, ch)
		case ch.IsMPIM:
			mpdms = append(mpdms, ch)
		case ch.IsExternal:
			externals = append(externals, ch)
		default:
			channels = append(channels, ch)
		}
	}

	// Within each group, prioritize unread messages (for groups that aren't
	// the "Unread" group itself), then sort by newest activity. Channels
	// with no known LatestTS fall to the bottom in stable name order.
	byActivity := func(group []slack.Channel) {
		sort.SliceStable(group, func(i, j int) bool {
			ci, cj := group[i], group[j]
			// Hidden channels only surface here when they have new activity, so
			// float them to the top of the category -- otherwise they'd be lost
			// behind always-visible channels.
			hi, hj := s.hidden[ci.ID], s.hidden[cj.ID]
			if hi != hj {
				return hi
			}
			// Prioritize unread count (relevant for Favorites and category groups)
			if (ci.UnreadCount > 0) != (cj.UnreadCount > 0) {
				return ci.UnreadCount > 0
			}
			// Then sort by latest timestamp
			if ci.LatestTS != cj.LatestTS {
				if ci.LatestTS == "" {
					return false
				}
				if cj.LatestTS == "" {
					return true
				}
				return ci.LatestTS > cj.LatestTS
			}
			return ci.Name < cj.Name
		})
	}

	byActivity(favs)
	if hasAnyUnread {
		unread = []slack.Channel{{ID: "__UNREADS__", Name: "All Unreads"}}
	}
	byActivity(channels)
	byActivity(externals)
	byActivity(dms)
	byActivity(mpdms)

	groups := []struct {
		header string
		items  []slack.Channel
		fav    bool
		limit  bool
	}{
		{"Unread", unread, false, false},
		{"Favorites", favs, true, false},
		{"Channels", channels, false, true},
		{"External", externals, false, true},
		{"Direct messages", dms, false, true},
		{"Group messages", mpdms, false, true},
	}

	rows := make([]*sidebarRow, 0, len(s.raw)+len(groups))
	for _, g := range groups {
		if len(g.items) == 0 {
			continue
		}
		collapsed := s.collapsed[g.header]
		hr, ok := old[headerKey(g.header)]
		if !ok {
			hr = &sidebarRow{kind: rowHeader}
		}
		hr.kind = rowHeader
		hr.header = g.header
		hr.headerKey = g.header
		hr.collapsed = collapsed
		rows = append(rows, hr)
		if collapsed {
			continue
		}

		items := g.items
		if s.showOnlyRecent && g.limit && len(items) > 10 {
			items = items[:10]
		}

		for _, ch := range items {
			r, ok := old[ch.ID]
			if !ok {
				r = &sidebarRow{kind: rowChannel}
			}
			r.kind = rowChannel
			r.channel = ch
			r.isFavorite = g.fav
			rows = append(rows, r)
		}
	}
	s.rows = rows
}

// headerKey distinguishes header rows from channel IDs in the old-row lookup
// so a click state is preserved across rebuilds without colliding with a
// channel that happens to share the header text.
func headerKey(h string) string { return "__hdr:" + h }

// Layout draws the sidebar.
func (s *ChannelsSidebar) Layout(gtx layout.Context, th *Theme, fm *slack.Formatter) layout.Dimensions {
	s.mu.Lock()
	if s.showOnlyRecent != th.ShowOnlyRecentChannels {
		s.showOnlyRecent = th.ShowOnlyRecentChannels
		s.dirty = true
	}
	if s.hideEmpty != th.HideEmptyChannels {
		s.hideEmpty = th.HideEmptyChannels
		s.dirty = true
	}

	if s.dirty && s.raw != nil {
		s.rebuildRowsLocked()
		s.dirty = false
	}

	// Process row clicks.
	var toggleHeader string
	for _, r := range s.rows {
		switch r.kind {
		case rowChannel:
			if r.click.Clicked(gtx) {
				s.activeID = r.channel.ID
				s.cursorKey = r.channel.ID
				if s.onSelect != nil {
					onSelect := s.onSelect
					id := r.channel.ID
					s.mu.Unlock()
					onSelect(id)
					s.mu.Lock()
				}
			}
		case rowHeader:
			if r.click.Clicked(gtx) {
				toggleHeader = r.headerKey
				s.cursorKey = headerKey(r.headerKey)
			}
		}
	}
	if toggleHeader != "" {
		s.collapsed[toggleHeader] = !s.collapsed[toggleHeader]
		s.rebuildRowsLocked()
		if s.onCollapsedChanged != nil {
			onCollapsedChanged := s.onCollapsedChanged
			collapsed := s.collapsedSliceLocked()
			s.mu.Unlock()
			onCollapsedChanged(collapsed)
			s.mu.Lock()
		}
	}

	n := len(s.rows)
	s.mu.Unlock()

	return withBorder(gtx, th.SidebarPal.Border, borders{Right: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.SidebarPal.BgSidebar, func(gtx layout.Context) layout.Dimensions {
			return material.List(th.Mat, &s.list).Layout(gtx, n, func(gtx layout.Context, idx int) layout.Dimensions {
				s.mu.Lock()
				if idx >= len(s.rows) {
					s.mu.Unlock()
					return layout.Dimensions{}
				}
				r := s.rows[idx]
				s.mu.Unlock()
				if r.kind == rowHeader {
					return s.layoutHeader(gtx, th, r)
				}
				return s.layoutRow(gtx, th, fm, r)
			})
		})
	})
}

func (s *ChannelsSidebar) layoutHeader(gtx layout.Context, th *Theme, r *sidebarRow) layout.Dimensions {
	chevron := "v "
	if r.collapsed {
		chevron = "> "
	}
	s.mu.Lock()
	selected := s.cursorKey == headerKey(r.headerKey)
	s.mu.Unlock()

	bg := th.SidebarPal.BgSidebar
	textColor := th.SidebarPal.TextMuted
	if selected {
		bg = th.SidebarPal.BgRowAlt
		textColor = th.SidebarPal.TextDim
	}
	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(14),
				Bottom: unit.Dp(4),
				Left:   unit.Dp(12),
				Right:  unit.Dp(12),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Caption(th.Mat, chevron+strings.ToUpper(r.header))
				lbl.Color = textColor
				lbl.Font.Weight = font.SemiBold
				lbl.TextSize = unit.Sp(11)
				th.applyFont(&lbl, th.Fonts.Channels)
				// Re-apply the smaller size if not explicitly overridden in Channels section
				if th.Fonts.Channels.Size == 0 && th.Fonts.Global.Size == 0 {
					lbl.TextSize = unit.Sp(11)
				} else if th.Fonts.Channels.Size == 0 {
					lbl.TextSize = unit.Sp(max(8, th.Fonts.Global.Size-2))
				}
				return lbl.Layout(gtx)
			})
		})
	})
}

func (s *ChannelsSidebar) layoutRow(gtx layout.Context, th *Theme, fm *slack.Formatter, r *sidebarRow) layout.Dimensions {
	s.mu.Lock()
	active := r.channel.ID == s.activeID
	s.mu.Unlock()

	hasUnread := r.channel.UnreadCount > 0
	hasMention := r.channel.MentionCount > 0
	if r.channel.IsIM || r.channel.IsMPIM {
		if hasUnread {
			hasMention = true
		}
	}
	if r.channel.ID == "__UNREADS__" {
		hasUnread = true
		hasMention = true
	}

	bg := th.SidebarPal.BgSidebar
	textColor := th.SidebarPal.Text
	leftBorder := borders{}
	switch {
	case active:
		bg = th.SidebarPal.BgRowAlt
		textColor = th.SidebarPal.TextStrong
		leftBorder = borders{Left: true}
	case hasUnread:
		textColor = th.SidebarPal.TextStrong
	case r.channel.LatestTS == "":
		textColor = th.SidebarPal.TextMuted
	}

	var presence string
	prefix := channelPrefix(r.channel)
	if r.channel.IsIM && fm != nil {
		if u := fm.GetUser(r.channel.UserID); u != nil {
			presence = u.Presence
			if presence == "active" {
				prefix = "● "
			} else {
				prefix = "○ "
			}
		}
	}

	name := r.channel.Name
	if name == "" {
		name = r.channel.ID
	}

	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		drawAccentStripe := func(gtx layout.Context) layout.Dimensions {
			if leftBorder.Left {
				return withBorder(gtx, th.SidebarPal.Accent, leftBorder, func(gtx layout.Context) layout.Dimensions {
					return s.layoutRowInner(gtx, th, r, prefix, name, presence, textColor, hasUnread, hasMention, active, bg)
				})
			}
			return s.layoutRowInner(gtx, th, r, prefix, name, presence, textColor, hasUnread, hasMention, active, bg)
		}
		return drawAccentStripe(gtx)
	})
}

func (s *ChannelsSidebar) layoutRowInner(
	gtx layout.Context,
	th *Theme,
	r *sidebarRow,
	prefix, name, presence string,
	textColor color.NRGBA,
	hasUnread, hasMention, active bool,
	bg color.NRGBA,
) layout.Dimensions {
	return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(5),
			Bottom: unit.Dp(5),
			Left:   unit.Dp(12),
			Right:  unit.Dp(12),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body1(th.Mat, prefix)
					lbl.Color = th.SidebarPal.TextMuted
					if hasUnread || active {
						lbl.Color = textColor
						lbl.Font.Weight = font.Bold
					}
					if presence == "active" {
						lbl.Color = th.SidebarPal.PresenceActive
					} else if presence == "away" {
						lbl.Color = th.SidebarPal.PresenceAway
					} else if active {
						lbl.Color = th.SidebarPal.TextDim
					}
					th.applyFont(&lbl, th.Fonts.Channels)
					return lbl.Layout(gtx)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body1(th.Mat, name)
					lbl.Color = textColor
					if hasUnread || active {
						lbl.Font.Weight = font.Bold
					}
					th.applyFont(&lbl, th.Fonts.Channels)
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					// For DMs, we show everything as a mention (red), so hide the gray unread badge
					if r.channel.ID == "__UNREADS__" || r.channel.UnreadCount <= 0 || r.channel.IsIM || r.channel.IsMPIM {
						return layout.Dimensions{}
					}
					lbl := material.Caption(th.Mat, fmt.Sprintf("%d", r.channel.UnreadCount))
					lbl.Color = th.SidebarPal.UnreadBadge
					lbl.Font.Weight = font.SemiBold
					th.applyFont(&lbl, th.Fonts.Channels)
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					count := r.channel.MentionCount
					if r.channel.IsIM || r.channel.IsMPIM {
						count = r.channel.UnreadCount
					}

					if r.channel.ID == "__UNREADS__" || count <= 0 {
						return layout.Dimensions{}
					}
					return layout.Inset{Left: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Caption(th.Mat, fmt.Sprintf("%d", count))
						lbl.Color = th.SidebarPal.MentionBadge
						lbl.Font.Weight = font.Bold
						th.applyFont(&lbl, th.Fonts.Channels)
						return lbl.Layout(gtx)
					})
				}),
			)
		})
	})
}

func channelPrefix(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		return "@ "
	case ch.IsMPIM:
		return ""
	case ch.IsExternal:
		return "~ "
	case ch.IsPrivate:
		return "* "
	default:
		return "# "
	}
}
