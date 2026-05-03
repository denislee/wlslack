package ui

import (
	"fmt"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// LinkPicker is the overlay that appears when the user presses Enter on a
// message containing more than one URL. Selecting a row hands the URL back
// through onSelect; the host is responsible for actually opening it.
type LinkPicker struct {
	list     widget.List
	rows     []*linkRow
	selected int
	onSelect func(url string)
}

type linkRow struct {
	url   string
	click widget.Clickable
}

func newLinkPicker(onSelect func(string)) *LinkPicker {
	lp := &LinkPicker{onSelect: onSelect}
	lp.list.Axis = layout.Vertical
	return lp
}

// SetURLs replaces the picker's contents and resets the highlight to the top.
func (l *LinkPicker) SetURLs(urls []string) {
	rows := make([]*linkRow, 0, len(urls))
	for _, u := range urls {
		rows = append(rows, &linkRow{url: u})
	}
	l.rows = rows
	l.selected = 0
	l.list.Position = layout.Position{}
}

// MoveSelection shifts the highlight by delta, scrolling so the new row stays
// visible.
func (l *LinkPicker) MoveSelection(delta int) {
	if len(l.rows) == 0 {
		return
	}
	l.selected += delta
	if l.selected < 0 {
		l.selected = 0
	}
	if l.selected >= len(l.rows) {
		l.selected = len(l.rows) - 1
	}
	pos := &l.list.Position
	if pos.Count <= 0 {
		pos.First = l.selected
		pos.Offset = 0
	} else if l.selected < pos.First {
		pos.First = l.selected
		pos.Offset = 0
	} else if l.selected >= pos.First+pos.Count {
		pos.First = l.selected - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
}

// Submit fires onSelect for the currently highlighted URL, if any.
func (l *LinkPicker) Submit() {
	if l.selected < 0 || l.selected >= len(l.rows) {
		return
	}
	if l.onSelect != nil {
		l.onSelect(l.rows[l.selected].url)
	}
}

// Layout draws the picker. It owns a vertical list of URL rows with the
// highlighted one painted in the accent color.
func (l *LinkPicker) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	for i, r := range l.rows {
		if r.click.Clicked(gtx) {
			l.selected = i
			l.Submit()
		}
	}
	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(12),
			Bottom: unit.Dp(12),
			Left:   unit.Dp(16),
			Right:  unit.Dp(16),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					title := material.Subtitle1(th.Mat, "Open link")
					title.Color = th.Pal.Text
					title.Font.Weight = font.Bold
					return title.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := material.Caption(th.Mat, "j/k or ↑/↓ select · Enter open · Esc cancel")
					hint.Color = th.Pal.TextDim
					return layout.Inset{Top: unit.Dp(2), Bottom: unit.Dp(8)}.Layout(gtx, hint.Layout)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return material.List(th.Mat, &l.list).Layout(gtx, len(l.rows), func(gtx layout.Context, idx int) layout.Dimensions {
						return l.layoutRow(gtx, th, idx, l.rows[idx])
					})
				}),
			)
		})
	})
}

func (l *LinkPicker) layoutRow(gtx layout.Context, th *Theme, idx int, r *linkRow) layout.Dimensions {
	active := idx == l.selected
	bg := th.Pal.Bg
	color := th.Pal.Link
	if active {
		bg = th.Pal.Accent
		color = th.Pal.AccentText
	}
	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(6),
				Bottom: unit.Dp(6),
				Left:   unit.Dp(10),
				Right:  unit.Dp(10),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th.Mat, fmt.Sprintf("%d. %s", idx+1, r.url))
				lbl.Color = color
				if active {
					lbl.Font.Weight = font.Bold
				}
				return lbl.Layout(gtx)
			})
		})
	})
}
