package ui

import (
	"image"
	"strings"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// ReactionPicker is the overlay opened by lowercase 'r' on a selected
// message. The user types to filter the emoji catalog and Enter applies the
// highlighted emoji as a reaction.
type ReactionPicker struct {
	editor     widget.Editor
	list       widget.List
	all        []slack.EmojiEntry
	existing   []string
	reactorMap map[string]string
	rows       []*reactionRow
	selected   int
	lastQuery  string
	onSelect   func(name string)
	images     *slack.ImageLoader
}

type reactionRow struct {
	click    widget.Clickable
	entry    slack.EmojiEntry
	reactors string
}

func newReactionPicker(images *slack.ImageLoader, onSelect func(string)) *ReactionPicker {
	rp := &ReactionPicker{images: images, onSelect: onSelect}
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
func (r *ReactionPicker) Reset(detailed []slack.Reaction, fm *slack.Formatter) {
	r.editor.SetText("")
	r.lastQuery = ""
	r.selected = 0
	r.existing = nil
	r.reactorMap = make(map[string]string)
	for _, dr := range detailed {
		r.existing = append(r.existing, dr.Name)
		reactors := make([]string, 0, len(dr.Users))
		for _, uID := range dr.Users {
			name := uID
			if u := fm.GetUser(uID); u != nil {
				name = u.Name
				if u.DisplayName != "" {
					name = u.DisplayName
				}
			}
			reactors = append(reactors, name)
		}
		if len(reactors) > 0 {
			r.reactorMap[dr.Name] = strings.Join(reactors, ", ")
		}
	}

	r.list.Position.First = 0
	r.list.Position.Offset = 0
	r.refilter()
}

// Editor exposes the input widget so the host can manage focus on it.
func (r *ReactionPicker) Editor() *widget.Editor { return &r.editor }

// DeleteLastWord deletes the last word in the editor, simulating Ctrl+W.
func (r *ReactionPicker) DeleteLastWord() {
	_, end := r.editor.Selection()
	if end == 0 {
		return
	}

	runes := []rune(r.editor.Text())
	if end > len(runes) {
		end = len(runes)
	}

	isSep := func(ru rune) bool {
		return ru == ' ' || ru == '\t'
	}

	pos := end
	// Skip trailing whitespace
	for pos > 0 && isSep(runes[pos-1]) {
		pos--
	}
	// Skip the word
	for pos > 0 && !isSep(runes[pos-1]) {
		pos--
	}

	if pos != end {
		r.editor.SetCaret(pos, end)
		r.editor.Insert("")
	}
}

func (r *ReactionPicker) MoveToStart() {
	r.editor.SetCaret(0, 0)
}

func (r *ReactionPicker) MoveToEnd() {
	n := len([]rune(r.editor.Text()))
	r.editor.SetCaret(n, n)
}

func (r *ReactionPicker) MoveCursor(delta int) {
	_, end := r.editor.Selection()
	n := len([]rune(r.editor.Text()))
	newPos := end + delta
	if newPos < 0 {
		newPos = 0
	}
	if newPos > n {
		newPos = n
	}
	r.editor.SetCaret(newPos, newPos)
}

func (r *ReactionPicker) Clear() {
	r.editor.SetText("")
}

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

	existingMap := make(map[string]bool)
	for _, name := range r.existing {
		existingMap[name] = true
	}

	for _, e := range r.all {
		if existingMap[e.Name] {
			if query == "" || strings.Contains(e.Name, query) {
				out = append(out, &reactionRow{entry: e, reactors: r.reactorMap[e.Name]})
			}
		}
	}

	for _, e := range r.all {
		if !existingMap[e.Name] {
			if query == "" || strings.Contains(e.Name, query) {
				out = append(out, &reactionRow{entry: e})
			}
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
								ed.SelectionColor = WithAlpha(th.Pal.Selection, 0x66)
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
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if strings.HasPrefix(row.entry.Glyph, "http") {
							imgOp, ok, done := r.images.GetOp(row.entry.Glyph)
							if !done {
								gtx.Constraints.Min = image.Point{X: gtx.Dp(20), Y: gtx.Dp(20)}
								return layout.Dimensions{Size: gtx.Constraints.Min}
							}
							if ok {
								target := gtx.Dp(20)
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
						glyph := material.Body1(th.Mat, row.entry.Glyph)
						glyph.Color = color
						if active {
							glyph.Font.Weight = font.SemiBold
						}
						return glyph.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						text := ":" + row.entry.Name + ":"
						if row.reactors != "" {
							text += " (" + row.reactors + ")"
						}
						lbl := material.Body1(th.Mat, text)
						lbl.Color = color
						if active {
							lbl.Font.Weight = font.SemiBold
						}
						return lbl.Layout(gtx)
					}),
				)
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
