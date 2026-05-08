package ui

import (
	"image"
	"sort"
	"strings"
	"sync"
	"time"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

type switcherTab int

const (
	tabChannels switcherTab = iota
	tabMessages
)

// QuickSwitcher is the Ctrl+K jump-to-channel overlay. It owns its own editor
// and result list; the host (App) feeds it the full channel set and listens
// for key events to drive selection / submit.
type QuickSwitcher struct {
	mu        sync.Mutex
	dirty     bool
	editor    widget.Editor
	list      widget.List
	all       []slack.Channel
	results   []slack.SearchResult
	rows      []*switcherRow
	selected  int
	lastQuery string
	tab       switcherTab
	onSelect  func(channelID string, messageTS string)
	onSearch  func(query string)
}

type switcherRow struct {
	click   widget.Clickable
	channel slack.Channel
	result  slack.SearchResult
}

func newQuickSwitcher(onSelect func(string, string), onSearch func(string)) *QuickSwitcher {
	qs := &QuickSwitcher{onSelect: onSelect, onSearch: onSearch}
	qs.editor.SingleLine = true
	qs.list.Axis = layout.Vertical
	return qs
}

// SetChannels stores the unfiltered channel set used as the search corpus.
func (q *QuickSwitcher) SetChannels(channels []slack.Channel) {
	q.mu.Lock()
	q.all = channels
	q.dirty = true
	q.mu.Unlock()
}

// SetResults stores the message search results.
func (q *QuickSwitcher) SetResults(results []slack.SearchResult) {
	q.mu.Lock()
	q.results = results
	q.dirty = true
	q.mu.Unlock()
}

// Reset clears the query and selection. Call when the switcher opens.
func (q *QuickSwitcher) Reset() {
	q.editor.SetText("")
	q.mu.Lock()
	q.lastQuery = ""
	q.selected = 0
	q.list.Position.First = 0
	q.list.Position.Offset = 0
	q.tab = tabChannels
	q.mu.Unlock()
	q.refilter()
}

// Editor exposes the input widget so the host can manage focus on it.
func (q *QuickSwitcher) Editor() *widget.Editor { return &q.editor }

// ToggleTab switches between Channels and Messages search.
func (q *QuickSwitcher) ToggleTab() {
	q.mu.Lock()
	if q.tab == tabChannels {
		q.tab = tabMessages
	} else {
		q.tab = tabChannels
	}
	q.selected = 0
	q.list.Position.First = 0
	q.list.Position.Offset = 0
	q.mu.Unlock()
	q.refilter()
}

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

func (q *QuickSwitcher) SelectAll() {
	n := len([]rune(q.editor.Text()))
	q.editor.SetCaret(0, n)
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

func (q *QuickSwitcher) MoveWord(dir int) {
	MoveWord(&q.editor, dir)
}

func (q *QuickSwitcher) Clear() {
	q.editor.SetText("")
}

// MoveSelection shifts the highlighted row, scrolling to keep it in view.
func (q *QuickSwitcher) MoveSelection(delta int) {
	q.mu.Lock()
	defer q.mu.Unlock()
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
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.selected < 0 || q.selected >= len(q.rows) {
		return
	}
	if q.onSelect != nil {
		row := q.rows[q.selected]
		if q.tab == tabChannels {
			q.onSelect(row.channel.ID, "")
		} else {
			q.onSelect(row.result.ChannelID, row.result.Message.Timestamp)
		}
	}
}

func (q *QuickSwitcher) refilter() {
	query := strings.TrimSpace(strings.ToLower(q.editor.Text()))

	q.mu.Lock()
	changed := query != q.lastQuery
	q.lastQuery = query
	tab := q.tab
	all := q.all
	results := q.results
	q.mu.Unlock()

	if tab == tabMessages && changed && query != "" {
		if q.onSearch != nil {
			q.onSearch(query)
		}
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	out := q.rows[:0]
	if q.tab == tabChannels {
		type scored struct {
			row   *switcherRow
			score int
		}
		scoredRows := make([]scored, 0, len(all))
		for _, ch := range all {
			if query == "" {
				scoredRows = append(scoredRows, scored{row: &switcherRow{channel: ch}})
				continue
			}
			score, ok := fuzzyScore(query, strings.ToLower(ch.Name))
			if !ok {
				continue
			}
			scoredRows = append(scoredRows, scored{row: &switcherRow{channel: ch}, score: score})
		}
		if query != "" {
			sort.SliceStable(scoredRows, func(i, j int) bool {
				return scoredRows[i].score > scoredRows[j].score
			})
		}
		for _, s := range scoredRows {
			out = append(out, s.row)
		}
	} else {
		for _, res := range results {
			out = append(out, &switcherRow{result: res})
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

	q.mu.Lock()
	dirty := q.dirty
	q.dirty = false
	lastQuery := q.lastQuery
	q.mu.Unlock()

	if dirty || strings.TrimSpace(strings.ToLower(q.editor.Text())) != lastQuery {
		q.refilter()
	}

	q.mu.Lock()
	var clickedRow int = -1
	for i, r := range q.rows {
		if r.click.Clicked(gtx) {
			clickedRow = i
			break
		}
	}
	if clickedRow != -1 {
		q.selected = clickedRow
		q.mu.Unlock()
		q.Submit()
		q.mu.Lock()
	}
	defer q.mu.Unlock()

	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(16),
			Bottom: unit.Dp(16),
			Left:   unit.Dp(20),
			Right:  unit.Dp(20),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return q.layoutTab(gtx, th, "Channels", q.tab == tabChannels)
						}),
						layout.Rigid(layout.Spacer{Width: unit.Dp(12)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return q.layoutTab(gtx, th, "Messages", q.tab == tabMessages)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(12)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
						return paintedBg(gtx, th.Pal.BgCode, func(gtx layout.Context) layout.Dimensions {
							return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								hint := "Jump to channel or DM..."
								if q.tab == tabMessages {
									hint = "Search messages..."
								}
								ed := material.Editor(th.Mat, &q.editor, hint)
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
					if len(q.rows) == 0 && q.tab == tabMessages && lastQuery != "" {
						return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body1(th.Mat, "No messages found.")
							th.applyFont(&lbl, FontStyle{})
							lbl.Color = th.Pal.TextDim
							return lbl.Layout(gtx)
						})
					}
					return material.List(th.Mat, &q.list).Layout(gtx, len(q.rows), func(gtx layout.Context, idx int) layout.Dimensions {
						return q.layoutRow(gtx, th, idx, q.rows[idx])
					})
				}),
			)
		})
	})
}

func (q *QuickSwitcher) layoutTab(gtx layout.Context, th *Theme, label string, active bool) layout.Dimensions {
	color := th.Pal.TextMuted
	if active {
		color = th.Pal.Accent
	}
	return layout.Stack{}.Layout(gtx,
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{Bottom: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				lbl := material.Body2(th.Mat, label)
				th.applyFont(&lbl, FontStyle{})
				lbl.Color = color
				if active {
					lbl.Font.Weight = font.Bold
				}
				return lbl.Layout(gtx)
			})
		}),
		layout.Stacked(func(gtx layout.Context) layout.Dimensions {
			if !active {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: unit.Dp(20)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				rect := image.Rect(0, 0, gtx.Constraints.Min.X, gtx.Dp(unit.Dp(2)))
				paint.FillShape(gtx.Ops, color, clip.Rect(rect).Op())
				return layout.Dimensions{Size: rect.Size()}
			})
		}),
	)
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
				if q.tab == tabChannels {
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
				} else {
					// Message search result
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Body2(th.Mat, r.result.ChannelName)
									th.applyFont(&lbl, FontStyle{})
									lbl.Color = th.Pal.Accent
									lbl.Font.Style = font.Italic
									return lbl.Layout(gtx)
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									lbl := material.Caption(th.Mat, r.result.Message.Username)
									th.applyFont(&lbl, FontStyle{})
									lbl.Color = th.Pal.TextStrong
									lbl.Font.Weight = font.Bold
									return lbl.Layout(gtx)
								}),
								layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									var ageStr string
									if t, ok := slack.ParseTimestamp(r.result.Message.Timestamp); ok {
										ageStr = slack.FormatAge(time.Since(t))
									}
									lbl := material.Caption(th.Mat, ageStr)
									th.applyFont(&lbl, FontStyle{})
									lbl.Color = th.Pal.Text
									return lbl.Layout(gtx)
								}),
							)
						}),
						layout.Rigid(layout.Spacer{Height: unit.Dp(2)}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							// Clean up message text (Slack search returns some weird characters sometimes)
							text := strings.ReplaceAll(r.result.Message.Text, "\n", " ")
							if len(text) > 200 {
								text = text[:197] + "..."
							}
							lbl := material.Body2(th.Mat, text)
							lbl.Color = color
							lbl.MaxLines = 1
							th.applyFont(&lbl, th.Fonts.Search)
							return lbl.Layout(gtx)
						}),
					)
				}
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

// fuzzyScore returns a match score for query against target (both expected to
// be lowercased). Both arguments are walked as runes so unicode names work.
// Higher scores mean better matches; ok is false when query's characters
// can't be found as an in-order subsequence of target.
//
// Bonuses: matching at the start of target, matching right after a separator
// (space, '-', '_', '.', '/'), and consecutive matches. A length penalty
// prefers tighter matches when scores would otherwise tie.
func fuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := []rune(query)
	t := []rune(target)
	score := 0
	qi := 0
	prevMatch := -2
	consecutive := 0
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if q[qi] != t[ti] {
			continue
		}
		score += 10
		if prevMatch == ti-1 {
			consecutive++
			score += 15 * consecutive
		} else {
			consecutive = 0
		}
		if ti == 0 {
			score += 35
		} else {
			switch t[ti-1] {
			case ' ', '-', '_', '.', '/':
				score += 30
			}
		}
		if ti == qi {
			score += 5
		}
		prevMatch = ti
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	score -= len(t) - len(q)
	return score, true
}
