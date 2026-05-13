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
// message and there's more than one thing that could be opened: multiple URLs,
// or a mix of URL(s) and image attachment(s). Each row carries its own pick
// action so the picker doesn't need to know whether it's opening a browser or
// the in-app image viewer.
type LinkPicker struct {
	list     widget.List
	rows     []*pickerRow
	selected int
	title    string
}

type pickerRow struct {
	label  string
	onPick func()
	click  widget.Clickable
}

func newLinkPicker() *LinkPicker {
	lp := &LinkPicker{}
	lp.list.Axis = layout.Vertical
	return lp
}

// SetItems replaces the picker's contents and resets the highlight to the top.
// title is shown above the list (e.g. "Open link" or "Open").
func (l *LinkPicker) SetItems(title string, rows []pickerRow) {
	r := make([]*pickerRow, 0, len(rows))
	for i := range rows {
		row := rows[i]
		r = append(r, &row)
	}
	l.title = title
	l.rows = r
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

// Submit fires the pick action for the currently highlighted row, if any.
func (l *LinkPicker) Submit() {
	if l.selected < 0 || l.selected >= len(l.rows) {
		return
	}
	if fn := l.rows[l.selected].onPick; fn != nil {
		fn()
	}
}

// Layout draws the picker. It owns a vertical list of rows with the
// highlighted one painted in the accent color.
func (l *LinkPicker) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	for i, r := range l.rows {
		if r.click.Clicked(gtx) {
			l.selected = i
			l.Submit()
		}
	}
	title := l.title
	if title == "" {
		title = "Open"
	}
	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(16),
			Bottom: unit.Dp(16),
			Left:   unit.Dp(20),
			Right:  unit.Dp(20),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					t := material.Subtitle1(th.Mat, title)
					th.applyFont(&t, FontStyle{})
					t.Color = th.Pal.TextStrong
					t.Font.Weight = font.SemiBold
					return t.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := material.Caption(th.Mat, "j/k or ^/v select | Enter open | h/q/Esc cancel")
					th.applyFont(&hint, FontStyle{})
					hint.Color = th.Pal.TextMuted
					return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(12)}.Layout(gtx, hint.Layout)
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

func (l *LinkPicker) layoutRow(gtx layout.Context, th *Theme, idx int, r *pickerRow) layout.Dimensions {
	active := idx == l.selected
	bg := th.Pal.Bg
	color := th.Pal.Link
	if active {
		bg = th.Pal.BgRowAlt
		color = th.Pal.TextStrong
	}
	row := func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(7),
				Bottom: unit.Dp(7),
				Left:   unit.Dp(12),
				Right:  unit.Dp(12),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th.Mat, fmt.Sprintf("%d. %s", idx+1, r.label))
				th.applyFont(&lbl, FontStyle{})
				lbl.Color = color
				if active {
					lbl.Font.Weight = font.SemiBold
				}
				return lbl.Layout(gtx)
			})
		})
	}
	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if active {
			return withBorder(gtx, th.Pal.Accent, borders{Left: true}, row)
		}
		return row(gtx)
	})
}
