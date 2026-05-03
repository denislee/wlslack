package ui

import (
	"fmt"
	"sort"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// ChannelsSidebar renders the left-hand list of channels.
type ChannelsSidebar struct {
	list     widget.List
	rows     []*sidebarRow
	activeID string
	onSelect func(id string)

	// favorites is the set of channel IDs the user has starred. Updates flow
	// through the host App via onFavoritesChanged so the new set can be
	// persisted to disk.
	favorites          map[string]bool
	onFavoritesChanged func([]string)

	// raw retains the most recent unsorted channel set so we can re-group when
	// favorites change without waiting for the next poll.
	raw []slack.Channel
}

type rowKind int

const (
	rowChannel rowKind = iota
	rowHeader
)

type sidebarRow struct {
	kind       rowKind
	header     string
	click      widget.Clickable
	channel    slack.Channel
	isFavorite bool
}

func newChannelsSidebar(onSelect func(id string)) *ChannelsSidebar {
	cs := &ChannelsSidebar{
		onSelect:  onSelect,
		favorites: make(map[string]bool),
	}
	cs.list.Axis = layout.Vertical
	return cs
}

// SetActive marks the given channel ID as the highlighted one.
func (s *ChannelsSidebar) SetActive(id string) {
	s.activeID = id
}

// SetFavorites replaces the favorites set and registers a change callback.
// The callback fires whenever ToggleFavoriteOnActive flips a channel so the
// host can persist the new list.
func (s *ChannelsSidebar) SetFavorites(ids []string, onChanged func([]string)) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.favorites = set
	s.onFavoritesChanged = onChanged
	if s.raw != nil {
		s.rebuildRows()
	}
}

// ToggleFavoriteOnActive flips the favorite state of the highlighted channel.
// Returns true if a change was made.
func (s *ChannelsSidebar) ToggleFavoriteOnActive() bool {
	if s.activeID == "" {
		return false
	}
	if s.favorites[s.activeID] {
		delete(s.favorites, s.activeID)
	} else {
		s.favorites[s.activeID] = true
	}
	s.rebuildRows()
	if s.onFavoritesChanged != nil {
		s.onFavoritesChanged(s.favoritesSlice())
	}
	return true
}

func (s *ChannelsSidebar) favoritesSlice() []string {
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
	for _, r := range s.rows {
		if r.kind == rowChannel {
			return r.channel.ID
		}
	}
	return ""
}

// PageSize returns the number of fully visible items minus one for overlap.
func (s *ChannelsSidebar) PageSize() int {
	if s.list.Position.Count > 1 {
		return s.list.Position.Count - 1
	}
	return 1
}

// MoveSelection shifts the highlighted row by delta (positive = down) and
// returns the newly active channel ID. The list is scrolled so the new row
// stays visible. Returns ("", false) when there are no rows yet. Header rows
// are skipped over so navigation always lands on a real channel.
func (s *ChannelsSidebar) MoveSelection(delta int) (string, bool) {
	channelIdxs := make([]int, 0, len(s.rows))
	for i, r := range s.rows {
		if r.kind == rowChannel {
			channelIdxs = append(channelIdxs, i)
		}
	}
	if len(channelIdxs) == 0 {
		return "", false
	}

	cur := -1
	for ci, ri := range channelIdxs {
		if s.rows[ri].channel.ID == s.activeID {
			cur = ci
			break
		}
	}
	if cur < 0 {
		if delta < 0 {
			cur = len(channelIdxs) - 1
		} else {
			cur = 0
		}
	} else {
		cur += delta
	}
	if cur < 0 {
		cur = 0
	}
	if cur >= len(channelIdxs) {
		cur = len(channelIdxs) - 1
	}
	idx := channelIdxs[cur]
	s.activeID = s.rows[idx].channel.ID

	// Keep the selection inside the visible window. Position.Count is the
	// number of items the list rendered last frame; before the first layout
	// it's 0, so just snap First to the new index in that case.
	pos := &s.list.Position
	if pos.Count <= 0 {
		pos.First = idx
		pos.Offset = 0
	} else if idx < pos.First {
		pos.First = idx
		pos.Offset = 0
	} else if idx >= pos.First+pos.Count {
		pos.First = idx - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
	return s.activeID, true
}

// SetChannels rebuilds the row list from the latest channel snapshot.
func (s *ChannelsSidebar) SetChannels(channels []slack.Channel) {
	s.raw = channels
	s.rebuildRows()
}

// rebuildRows splits channels into Favorites plus the four conversation-type
// groups (channels, external, DMs, group DMs) and sorts each. Click state for
// previously rendered channel rows is reused so highlights survive the rebuild.
func (s *ChannelsSidebar) rebuildRows() {
	old := make(map[string]*sidebarRow, len(s.rows))
	for _, r := range s.rows {
		if r.kind == rowChannel {
			old[r.channel.ID] = r
		}
	}

	// Unread aggregates channels with unread messages from all conversation
	// types so the user can scan them in one place. A channel only ever lives
	// in one group — being unread takes priority over its category.
	var unread, favs, channels, externals, dms, mpdms []slack.Channel
	for _, ch := range s.raw {
		switch {
		case ch.UnreadCount > 0:
			unread = append(unread, ch)
		case s.favorites[ch.ID]:
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

	byUnreadThenName := func(group []slack.Channel) {
		sort.SliceStable(group, func(i, j int) bool {
			ci, cj := group[i], group[j]
			if (ci.UnreadCount > 0) != (cj.UnreadCount > 0) {
				return ci.UnreadCount > 0
			}
			if ci.UnreadCount != cj.UnreadCount {
				return ci.UnreadCount > cj.UnreadCount
			}
			return ci.Name < cj.Name
		})
	}
	sort.SliceStable(favs, func(i, j int) bool { return favs[i].Name < favs[j].Name })
	byUnreadThenName(unread)
	byUnreadThenName(channels)
	byUnreadThenName(externals)
	byUnreadThenName(dms)
	byUnreadThenName(mpdms)

	groups := []struct {
		header string
		items  []slack.Channel
		fav    bool
	}{
		{"● Unread", unread, false},
		{"★ Favorites", favs, true},
		{"Channels", channels, false},
		{"External channels", externals, false},
		{"Direct messages", dms, false},
		{"Group messages", mpdms, false},
	}

	rows := make([]*sidebarRow, 0, len(s.raw)+len(groups))
	for _, g := range groups {
		if len(g.items) == 0 {
			continue
		}
		rows = append(rows, &sidebarRow{kind: rowHeader, header: g.header})
		for _, ch := range g.items {
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

// Layout draws the sidebar.
func (s *ChannelsSidebar) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	// Process row clicks.
	for _, r := range s.rows {
		if r.kind != rowChannel {
			continue
		}
		if r.click.Clicked(gtx) {
			s.activeID = r.channel.ID
			if s.onSelect != nil {
				s.onSelect(r.channel.ID)
			}
		}
	}

	return paintedBg(gtx, th.Pal.BgSidebar, func(gtx layout.Context) layout.Dimensions {
		return material.List(th.Mat, &s.list).Layout(gtx, len(s.rows), func(gtx layout.Context, idx int) layout.Dimensions {
			r := s.rows[idx]
			if r.kind == rowHeader {
				return s.layoutHeader(gtx, th, r.header)
			}
			return s.layoutRow(gtx, th, r)
		})
	})
}

func (s *ChannelsSidebar) layoutHeader(gtx layout.Context, th *Theme, text string) layout.Dimensions {
	return layout.Inset{
		Top:    unit.Dp(8),
		Bottom: unit.Dp(2),
		Left:   unit.Dp(10),
		Right:  unit.Dp(10),
	}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		lbl := material.Caption(th.Mat, text)
		lbl.Color = th.Pal.TextDim
		lbl.Font.Weight = font.Bold
		return lbl.Layout(gtx)
	})
}

func (s *ChannelsSidebar) layoutRow(gtx layout.Context, th *Theme, r *sidebarRow) layout.Dimensions {
	active := r.channel.ID == s.activeID
	bg := th.Pal.BgSidebar
	textColor := th.Pal.Text
	if active {
		bg = th.Pal.Accent
		textColor = th.Pal.AccentText
	} else if r.channel.UnreadCount > 0 {
		textColor = th.Pal.AccentText
	}

	prefix := channelPrefix(r.channel)
	name := r.channel.Name
	if name == "" {
		name = r.channel.ID
	}

	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(4),
				Bottom: unit.Dp(4),
				Left:   unit.Dp(10),
				Right:  unit.Dp(10),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th.Mat, prefix)
						lbl.Color = th.Pal.TextDim
						if active {
							lbl.Color = th.Pal.AccentText
						}
						return lbl.Layout(gtx)
					}),
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th.Mat, name)
						lbl.Color = textColor
						if r.channel.UnreadCount > 0 && !active {
							lbl.Font.Weight = th.BoldF.Weight
						}
						return lbl.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if r.channel.UnreadCount <= 0 {
							return layout.Dimensions{}
						}
						lbl := material.Caption(th.Mat, fmt.Sprintf(" %d", r.channel.UnreadCount))
						lbl.Color = th.Pal.Unread
						if active {
							lbl.Color = th.Pal.AccentText
						}
						return lbl.Layout(gtx)
					}),
				)
			})
		})
	})
}

func channelPrefix(ch slack.Channel) string {
	switch {
	case ch.IsIM:
		return "@ "
	case ch.IsMPIM:
		return "@@ "
	case ch.IsExternal:
		return "🌐 "
	case ch.IsPrivate:
		return "🔒 "
	default:
		return "# "
	}
}
