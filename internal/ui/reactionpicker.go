package ui

import (
	"strings"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// ReactionPicker is the overlay opened by lowercase 'r' on a selected
// message. The user types to filter the emoji catalog and Enter applies the
// highlighted emoji as a reaction.
type ReactionPicker struct {
	editor    widget.Editor
	list      widget.List
	all       []slack.EmojiEntry
	rows      []*reactionRow
	selected  int
	lastQuery string
	onSelect  func(name string)
}

type reactionRow struct {
	click widget.Clickable
	entry slack.EmojiEntry
}

func newReactionPicker(onSelect func(string)) *ReactionPicker {
	rp := &ReactionPicker{onSelect: onSelect}
	rp.editor.SingleLine = true
	rp.list.Axis = layout.Vertical
	return rp
}

// SetEmojis stores the unfiltered catalog used as the search corpus.
func (r *ReactionPicker) SetEmojis(entries []slack.EmojiEntry) {
	r.all = entries
	r.refilter()
}

// Reset clears the query and selection. Call when the picker opens.
func (r *ReactionPicker) Reset() {
	r.editor.SetText("")
	r.lastQuery = ""
	r.selected = 0
	r.list.Position.First = 0
	r.list.Position.Offset = 0
	r.refilter()
}

// Editor exposes the input widget so the host can manage focus on it.
func (r *ReactionPicker) Editor() *widget.Editor { return &r.editor }

// MoveSelection shifts the highlighted row, scrolling to keep it in view.
func (r *ReactionPicker) MoveSelection(delta int) {
	if len(r.rows) == 0 {
		return
	}
	r.selected += delta
	if r.selected < 0 {
		r.selected = 0
	}
	if r.selected >= len(r.rows) {
		r.selected = len(r.rows) - 1
	}
	pos := &r.list.Position
	if pos.Count <= 0 {
		pos.First = r.selected
		pos.Offset = 0
	} else if r.selected < pos.First {
		pos.First = r.selected
		pos.Offset = 0
	} else if r.selected >= pos.First+pos.Count {
		pos.First = r.selected - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
}

// Submit fires onSelect for the currently highlighted emoji, if any.
func (r *ReactionPicker) Submit() {
	if r.selected < 0 || r.selected >= len(r.rows) {
		return
	}
	if r.onSelect != nil {
		r.onSelect(r.rows[r.selected].entry.Name)
	}
}

func (r *ReactionPicker) refilter() {
	query := strings.TrimSpace(strings.ToLower(r.editor.Text()))
	r.lastQuery = query
	out := r.rows[:0]
	for _, e := range r.all {
		if query == "" || strings.Contains(e.Name, query) {
			out = append(out, &reactionRow{entry: e})
		}
	}
	r.rows = out
	if r.selected >= len(r.rows) {
		r.selected = len(r.rows) - 1
	}
	if r.selected < 0 {
		r.selected = 0
	}
}

// Layout draws the picker: query input on top, filtered emoji list below.
func (r *ReactionPicker) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	for {
		_, ok := r.editor.Update(gtx)
		if !ok {
			break
		}
	}
	if strings.TrimSpace(strings.ToLower(r.editor.Text())) != r.lastQuery {
		r.refilter()
	}

	for i, row := range r.rows {
		if row.click.Clicked(gtx) {
			r.selected = i
			r.Submit()
		}
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
					title := material.Subtitle1(th.Mat, "Add reaction")
					title.Color = th.Pal.TextStrong
					title.Font.Weight = font.SemiBold
					return title.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := material.Caption(th.Mat, "type to filter · ↑/↓ select · Enter react · Esc cancel")
					hint.Color = th.Pal.TextMuted
					return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(12)}.Layout(gtx, hint.Layout)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
						return paintedBg(gtx, th.Pal.BgCode, func(gtx layout.Context) layout.Dimensions {
							return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								ed := material.Editor(th.Mat, &r.editor, "Search emoji…")
								ed.Color = th.Pal.TextStrong
								ed.HintColor = th.Pal.TextMuted
								ed.SelectionColor = withAlpha(th.Pal.Selection, 0x66)
								return ed.Layout(gtx)
							})
						})
					})
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return material.List(th.Mat, &r.list).Layout(gtx, len(r.rows), func(gtx layout.Context, idx int) layout.Dimensions {
						return r.layoutRow(gtx, th, idx, r.rows[idx])
					})
				}),
			)
		})
	})
}

func (r *ReactionPicker) layoutRow(gtx layout.Context, th *Theme, idx int, row *reactionRow) layout.Dimensions {
	active := idx == r.selected
	bg := th.Pal.Bg
	color := th.Pal.Text
	if active {
		bg = th.Pal.BgRowAlt
		color = th.Pal.TextStrong
	}
	rowFn := func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(7),
				Bottom: unit.Dp(7),
				Left:   unit.Dp(12),
				Right:  unit.Dp(12),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body1(th.Mat, row.entry.Glyph+"  :"+row.entry.Name+":")
				lbl.Color = color
				if active {
					lbl.Font.Weight = font.SemiBold
				}
				return lbl.Layout(gtx)
			})
		})
	}
	return row.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if active {
			return withBorder(gtx, th.Pal.Accent, borders{Left: true}, rowFn)
		}
		return rowFn(gtx)
	})
}
