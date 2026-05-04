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

// QuickSwitcher is the Ctrl+K jump-to-channel overlay. It owns its own editor
// and result list; the host (App) feeds it the full channel set and listens
// for key events to drive selection / submit.
type QuickSwitcher struct {
	editor    widget.Editor
	list      widget.List
	all       []slack.Channel
	rows      []*switcherRow
	selected  int
	lastQuery string
	onSelect  func(channelID string)
}

type switcherRow struct {
	click   widget.Clickable
	channel slack.Channel
}

func newQuickSwitcher(onSelect func(string)) *QuickSwitcher {
	qs := &QuickSwitcher{onSelect: onSelect}
	qs.editor.SingleLine = true
	qs.list.Axis = layout.Vertical
	return qs
}

// SetChannels stores the unfiltered channel set used as the search corpus.
func (q *QuickSwitcher) SetChannels(channels []slack.Channel) {
	q.all = channels
	q.refilter()
}

// Reset clears the query and selection. Call when the switcher opens.
func (q *QuickSwitcher) Reset() {
	q.editor.SetText("")
	q.lastQuery = ""
	q.selected = 0
	q.list.Position.First = 0
	q.list.Position.Offset = 0
	q.refilter()
}

// Editor exposes the input widget so the host can manage focus on it.
func (q *QuickSwitcher) Editor() *widget.Editor { return &q.editor }

// DeleteLastWord deletes the last word in the editor, simulating Ctrl+W.
func (q *QuickSwitcher) DeleteLastWord() {
	_, end := q.editor.Selection()
	if end == 0 {
		return
	}

	runes := []rune(q.editor.Text())
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
		q.editor.SetCaret(pos, end)
		q.editor.Insert("")
	}
}

func (q *QuickSwitcher) MoveToStart() {
	q.editor.SetCaret(0, 0)
}

func (q *QuickSwitcher) MoveToEnd() {
	n := len([]rune(q.editor.Text()))
	q.editor.SetCaret(n, n)
}

func (q *QuickSwitcher) MoveCursor(delta int) {
	_, end := q.editor.Selection()
	n := len([]rune(q.editor.Text()))
	newPos := end + delta
	if newPos < 0 {
		newPos = 0
	}
	if newPos > n {
		newPos = n
	}
	q.editor.SetCaret(newPos, newPos)
}

func (q *QuickSwitcher) Clear() {
	q.editor.SetText("")
}

// MoveSelection shifts the highlighted row, scrolling to keep it in view.
func (q *QuickSwitcher) MoveSelection(delta int) {
	if len(q.rows) == 0 {
		return
	}
	q.selected += delta
	if q.selected < 0 {
		q.selected = 0
	}
	if q.selected >= len(q.rows) {
		q.selected = len(q.rows) - 1
	}
	pos := &q.list.Position
	if pos.Count <= 0 {
		pos.First = q.selected
		pos.Offset = 0
	} else if q.selected < pos.First {
		pos.First = q.selected
		pos.Offset = 0
	} else if q.selected >= pos.First+pos.Count {
		pos.First = q.selected - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
}

// Submit fires onSelect for the currently highlighted row, if any.
func (q *QuickSwitcher) Submit() {
	if q.selected < 0 || q.selected >= len(q.rows) {
		return
	}
	if q.onSelect != nil {
		q.onSelect(q.rows[q.selected].channel.ID)
	}
}

func (q *QuickSwitcher) refilter() {
	query := strings.TrimSpace(strings.ToLower(q.editor.Text()))
	q.lastQuery = query
	out := q.rows[:0]
	for _, ch := range q.all {
		if query == "" || strings.Contains(strings.ToLower(ch.Name), query) {
			out = append(out, &switcherRow{channel: ch})
		}
	}
	q.rows = out
	if q.selected >= len(q.rows) {
		q.selected = len(q.rows) - 1
	}
	if q.selected < 0 {
		q.selected = 0
	}
}

// Layout draws the switcher: query input on top, filtered results below.
func (q *QuickSwitcher) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	// Drain editor events; we only care that the query is current.
	for {
		_, ok := q.editor.Update(gtx)
		if !ok {
			break
		}
	}
	if strings.TrimSpace(strings.ToLower(q.editor.Text())) != q.lastQuery {
		q.refilter()
	}

	for i, r := range q.rows {
		if r.click.Clicked(gtx) {
			q.selected = i
			q.Submit()
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
					return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
						return paintedBg(gtx, th.Pal.BgCode, func(gtx layout.Context) layout.Dimensions {
							return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								ed := material.Editor(th.Mat, &q.editor, "Jump to channel or DM…")
								ed.Color = th.Pal.TextStrong
								ed.HintColor = th.Pal.TextMuted
								ed.SelectionColor = WithAlpha(th.Pal.Selection, 0x66)
								th.applyEditorFont(&ed, th.Fonts.Search)
								return ed.Layout(gtx)
							})
						})
					})
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return material.List(th.Mat, &q.list).Layout(gtx, len(q.rows), func(gtx layout.Context, idx int) layout.Dimensions {
						return q.layoutRow(gtx, th, idx, q.rows[idx])
					})
				}),
			)
		})
	})
}

func (q *QuickSwitcher) layoutRow(gtx layout.Context, th *Theme, idx int, r *switcherRow) layout.Dimensions {
	active := idx == q.selected
	bg := th.Pal.Bg
	color := th.Pal.Text
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
				name := r.channel.Name
				if name == "" {
					name = r.channel.ID
				}
				lbl := material.Body1(th.Mat, channelPrefix(r.channel)+name)
				lbl.Color = color
				if active {
					lbl.Font.Weight = font.SemiBold
				}
				th.applyFont(&lbl, th.Fonts.Search)
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
