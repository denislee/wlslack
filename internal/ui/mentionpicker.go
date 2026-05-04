package ui

import (
	"sort"
	"strings"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

type MentionPicker struct {
	list     widget.List
	rows     []mentionRow
	selected int
	active   bool
	onSelect func(id, label string, isGroup bool)
}

type mentionRow struct {
	id      string
	label   string
	isGroup bool
	click   widget.Clickable
}

func newMentionPicker() *MentionPicker {
	m := &MentionPicker{}
	m.list.Axis = layout.Vertical
	return m
}

func (m *MentionPicker) Open(query string, fm *slack.Formatter, onSelect func(string, string, bool)) {
	m.active = true
	m.onSelect = onSelect
	m.refilter(query, fm)
}

func (m *MentionPicker) Close() {
	m.active = false
}

func (m *MentionPicker) Active() bool {
	return m.active && len(m.rows) > 0
}

func (m *MentionPicker) MoveSelection(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.selected += delta
	if m.selected < 0 {
		m.selected = 0
	}
	if m.selected >= len(m.rows) {
		m.selected = len(m.rows) - 1
	}
}

func (m *MentionPicker) Submit() {
	if m.selected < 0 || m.selected >= len(m.rows) {
		return
	}
	row := m.rows[m.selected]
	if m.onSelect != nil {
		m.onSelect(row.id, row.label, row.isGroup)
	}
}

func (m *MentionPicker) refilter(query string, fm *slack.Formatter) {
	query = strings.ToLower(query)
	m.rows = m.rows[:0]

	// Add special mentions
	specials := []struct{ id, label string }{
		{"here", "here"},
		{"channel", "channel"},
		{"everyone", "everyone"},
	}
	for _, s := range specials {
		if query == "" || strings.Contains(s.label, query) {
			m.rows = append(m.rows, mentionRow{id: s.id, label: s.label})
		}
	}

	// Add user groups
	groups := fm.GetAllUserGroups()
	sort.Slice(groups, func(i, j int) bool {
		return strings.ToLower(groups[i].Handle) < strings.ToLower(groups[j].Handle)
	})
	for _, g := range groups {
		if query == "" || strings.Contains(strings.ToLower(g.Handle), query) || strings.Contains(strings.ToLower(g.Name), query) {
			m.rows = append(m.rows, mentionRow{id: g.ID, label: g.Handle, isGroup: true})
		}
	}

	// Add users
	users := fm.GetAllUsers()
	sort.Slice(users, func(i, j int) bool {
		ni := users[i].DisplayName
		if ni == "" {
			ni = users[i].Name
		}
		nj := users[j].DisplayName
		if nj == "" {
			nj = users[j].Name
		}
		return strings.ToLower(ni) < strings.ToLower(nj)
	})
	for _, u := range users {
		if u.IsBot {
			continue
		}
		name := u.Name
		if u.DisplayName != "" {
			name = u.DisplayName
		}
		if query == "" || strings.Contains(strings.ToLower(u.Name), query) || strings.Contains(strings.ToLower(u.RealName), query) || strings.Contains(strings.ToLower(u.DisplayName), query) {
			m.rows = append(m.rows, mentionRow{id: u.ID, label: name})
		}
	}

	if m.selected >= len(m.rows) {
		m.selected = len(m.rows) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

func (m *MentionPicker) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if !m.Active() {
		return layout.Dimensions{}
	}

	for i := range m.rows {
		if m.rows[i].click.Clicked(gtx) {
			m.selected = i
			m.Submit()
		}
	}

	return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.Pal.BgRowAlt, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.Y = gtx.Dp(unit.Dp(200))
			return material.List(th.Mat, &m.list).Layout(gtx, len(m.rows), func(gtx layout.Context, idx int) layout.Dimensions {
				row := m.rows[idx]
				active := idx == m.selected
				bg := th.Pal.BgRowAlt
				if active {
					bg = th.Pal.BgRowSelected
				}
				return row.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
						return layout.Inset{
							Top:    unit.Dp(4),
							Bottom: unit.Dp(4),
							Left:   unit.Dp(8),
							Right:  unit.Dp(8),
						}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							label := "@" + row.label
							if row.isGroup {
								label = "@" + row.label + " (group)"
							}
							lbl := material.Body2(th.Mat, label)
							th.applyFont(&lbl, FontStyle{})
							lbl.Color = th.Pal.Text
							if active {
								lbl.Font.Weight = font.SemiBold
								lbl.Color = th.Pal.TextStrong
							}
							return lbl.Layout(gtx)
						})
					})
				})
			})
		})
	})
}
