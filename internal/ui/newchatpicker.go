package ui

import (
	"sort"
	"strconv"
	"strings"
	"sync"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// NewChatPicker is the "start a new chat" overlay. It fuzzy-searches workspace
// users and lets the operator pick one (→ DM) or several (→ MPIM) before
// submitting via onSubmit.
type NewChatPicker struct {
	mu        sync.Mutex
	editor    widget.Editor
	list      widget.List
	all       []slack.User
	selfID    string
	rows      []*newChatRow
	picked    map[string]slack.User // id → user, preserves all picked users even when filtered out
	pickedIDs []string              // pick order, used for the chip strip
	selected  int
	onSubmit  func(userIDs []string)
}

type newChatRow struct {
	user   slack.User
	click  widget.Clickable
	picked bool
}

func newNewChatPicker(onSubmit func([]string)) *NewChatPicker {
	p := &NewChatPicker{
		onSubmit: onSubmit,
		picked:   make(map[string]slack.User),
	}
	p.editor.SingleLine = true
	p.list.Axis = layout.Vertical
	return p
}

func (p *NewChatPicker) Editor() *widget.Editor { return &p.editor }

// SetUsers stores the candidate corpus. Bots and the self user are filtered out.
func (p *NewChatPicker) SetUsers(users []slack.User, selfID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.all = p.all[:0]
	for _, u := range users {
		if u.IsBot || u.ID == selfID || u.ID == slack.SlackbotID {
			continue
		}
		p.all = append(p.all, u)
	}
	p.selfID = selfID
}

// Reset clears the query, selection, and picked set. Call when opening.
func (p *NewChatPicker) Reset() {
	p.editor.SetText("")
	p.mu.Lock()
	p.picked = make(map[string]slack.User)
	p.pickedIDs = nil
	p.selected = 0
	p.list.Position.First = 0
	p.list.Position.Offset = 0
	p.mu.Unlock()
	p.refilter()
}

func (p *NewChatPicker) DeleteLastWord() {
	_, end := p.editor.Selection()
	if end == 0 {
		p.unpickLast()
		return
	}
	runes := []rune(p.editor.Text())
	if end > len(runes) {
		end = len(runes)
	}
	isSep := func(r rune) bool { return r == ' ' || r == '\t' }
	pos := end
	for pos > 0 && isSep(runes[pos-1]) {
		pos--
	}
	for pos > 0 && !isSep(runes[pos-1]) {
		pos--
	}
	if pos != end {
		p.editor.SetCaret(pos, end)
		p.editor.Insert("")
	}
}

// Backspace removes the trailing picked user when the query is empty,
// otherwise it lets the editor handle the keystroke normally.
func (p *NewChatPicker) Backspace() bool {
	if p.editor.Text() != "" {
		return false
	}
	return p.unpickLast()
}

func (p *NewChatPicker) unpickLast() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pickedIDs) == 0 {
		return false
	}
	last := p.pickedIDs[len(p.pickedIDs)-1]
	p.pickedIDs = p.pickedIDs[:len(p.pickedIDs)-1]
	delete(p.picked, last)
	return true
}

func (p *NewChatPicker) SelectAll() {
	n := len([]rune(p.editor.Text()))
	p.editor.SetCaret(0, n)
}

func (p *NewChatPicker) MoveToStart() {
	p.editor.SetCaret(0, 0)
}

func (p *NewChatPicker) MoveToEnd() {
	n := len([]rune(p.editor.Text()))
	p.editor.SetCaret(n, n)
}

func (p *NewChatPicker) MoveCursor(delta int) {
	_, end := p.editor.Selection()
	n := len([]rune(p.editor.Text()))
	pos := end + delta
	if pos < 0 {
		pos = 0
	}
	if pos > n {
		pos = n
	}
	p.editor.SetCaret(pos, pos)
}

func (p *NewChatPicker) MoveWord(dir int) {
	MoveWord(&p.editor, dir)
}

func (p *NewChatPicker) Clear() {
	p.editor.SetText("")
}

func (p *NewChatPicker) MoveSelection(delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.rows) == 0 {
		return
	}
	p.selected += delta
	if p.selected < 0 {
		p.selected = 0
	}
	if p.selected >= len(p.rows) {
		p.selected = len(p.rows) - 1
	}
	pos := &p.list.Position
	if pos.Count <= 0 {
		pos.First = p.selected
		pos.Offset = 0
	} else if p.selected < pos.First {
		pos.First = p.selected
		pos.Offset = 0
	} else if p.selected >= pos.First+pos.Count {
		pos.First = p.selected - pos.Count + 1
		if pos.First < 0 {
			pos.First = 0
		}
		pos.Offset = 0
	}
}

// TogglePicked flips membership of the highlighted user in the picked set.
func (p *NewChatPicker) TogglePicked() {
	p.mu.Lock()
	if p.selected < 0 || p.selected >= len(p.rows) {
		p.mu.Unlock()
		return
	}
	u := p.rows[p.selected].user
	if _, ok := p.picked[u.ID]; ok {
		delete(p.picked, u.ID)
		for i, id := range p.pickedIDs {
			if id == u.ID {
				p.pickedIDs = append(p.pickedIDs[:i], p.pickedIDs[i+1:]...)
				break
			}
		}
	} else {
		p.picked[u.ID] = u
		p.pickedIDs = append(p.pickedIDs, u.ID)
	}
	p.mu.Unlock()
	// Clear the query so the next type starts a fresh search after a pick.
	p.editor.SetText("")
	p.refilter()
}

// Submit fires onSubmit with the picked user IDs. If nothing is explicitly
// picked but a row is highlighted, that row's user is used (one-shot DM).
func (p *NewChatPicker) Submit() {
	p.mu.Lock()
	ids := append([]string(nil), p.pickedIDs...)
	if len(ids) == 0 && p.selected >= 0 && p.selected < len(p.rows) {
		ids = []string{p.rows[p.selected].user.ID}
	}
	cb := p.onSubmit
	p.mu.Unlock()
	if len(ids) > 0 && cb != nil {
		cb(ids)
	}
}

func (p *NewChatPicker) refilter() {
	query := strings.TrimSpace(strings.ToLower(p.editor.Text()))

	p.mu.Lock()
	defer p.mu.Unlock()

	type scored struct {
		u     slack.User
		score int
	}

	candidates := make([]scored, 0, len(p.all))
	for _, u := range p.all {
		// Don't surface already-picked users in the result list.
		if _, ok := p.picked[u.ID]; ok {
			continue
		}
		name := u.DisplayName
		if name == "" {
			name = u.Name
		}
		if query == "" {
			candidates = append(candidates, scored{u: u})
			continue
		}
		bestScore := 0
		matched := false
		for _, target := range []string{name, u.Name, u.RealName, u.DisplayName} {
			if target == "" {
				continue
			}
			if s, ok := fuzzyScore(query, strings.ToLower(target)); ok {
				if !matched || s > bestScore {
					bestScore = s
					matched = true
				}
			}
		}
		if matched {
			candidates = append(candidates, scored{u: u, score: bestScore})
		}
	}

	if query == "" {
		sort.SliceStable(candidates, func(i, j int) bool {
			ni := candidates[i].u.DisplayName
			if ni == "" {
				ni = candidates[i].u.Name
			}
			nj := candidates[j].u.DisplayName
			if nj == "" {
				nj = candidates[j].u.Name
			}
			return strings.ToLower(ni) < strings.ToLower(nj)
		})
	} else {
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].score > candidates[j].score
		})
	}

	p.rows = p.rows[:0]
	for _, c := range candidates {
		p.rows = append(p.rows, &newChatRow{user: c.u})
	}
	if p.selected >= len(p.rows) {
		p.selected = len(p.rows) - 1
	}
	if p.selected < 0 {
		p.selected = 0
	}
}

func (p *NewChatPicker) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	for {
		_, ok := p.editor.Update(gtx)
		if !ok {
			break
		}
	}

	// Keep results in sync with the query.
	p.refilter()

	p.mu.Lock()
	clicked := -1
	for i, r := range p.rows {
		if r.click.Clicked(gtx) {
			clicked = i
			break
		}
	}
	if clicked != -1 {
		p.selected = clicked
		p.mu.Unlock()
		p.TogglePicked()
		p.mu.Lock()
	}
	rowCount := len(p.rows)
	pickedCount := len(p.pickedIDs)
	pickedNames := make([]string, 0, pickedCount)
	for _, id := range p.pickedIDs {
		u := p.picked[id]
		nm := u.DisplayName
		if nm == "" {
			nm = u.Name
		}
		if nm == "" {
			nm = id
		}
		pickedNames = append(pickedNames, nm)
	}
	p.mu.Unlock()

	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(16),
			Bottom: unit.Dp(16),
			Left:   unit.Dp(20),
			Right:  unit.Dp(20),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					title := "New chat"
					if pickedCount == 1 {
						title = "New chat (1 person — Enter to open)"
					} else if pickedCount > 1 {
						title = "New group chat (" + strconv.Itoa(pickedCount) + " people — Enter to open)"
					}
					lbl := material.Body2(th.Mat, title)
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.Accent
					lbl.Font.Weight = font.Bold
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if pickedCount == 0 {
						return layout.Dimensions{}
					}
					return layout.Inset{Bottom: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body2(th.Mat, "Picked: "+strings.Join(pickedNames, ", "))
						th.applyFont(&lbl, FontStyle{})
						lbl.Color = th.Pal.TextStrong
						return lbl.Layout(gtx)
					})
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
						return paintedBg(gtx, th.Pal.BgCode, func(gtx layout.Context) layout.Dimensions {
							return layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								hint := "Type a name. Tab to add, Enter to open."
								ed := material.Editor(th.Mat, &p.editor, hint)
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
					if rowCount == 0 {
						return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							lbl := material.Body1(th.Mat, "No users found.")
							th.applyFont(&lbl, FontStyle{})
							lbl.Color = th.Pal.TextDim
							return lbl.Layout(gtx)
						})
					}
					return material.List(th.Mat, &p.list).Layout(gtx, rowCount, func(gtx layout.Context, idx int) layout.Dimensions {
						p.mu.Lock()
						if idx >= len(p.rows) {
							p.mu.Unlock()
							return layout.Dimensions{}
						}
						row := p.rows[idx]
						active := idx == p.selected
						p.mu.Unlock()
						return p.layoutRow(gtx, th, row, active)
					})
				}),
			)
		})
	})
}

func (p *NewChatPicker) layoutRow(gtx layout.Context, th *Theme, r *newChatRow, active bool) layout.Dimensions {
	bg := th.Pal.Bg
	color := th.Pal.Text
	if active {
		bg = th.Pal.BgRowAlt
		color = th.Pal.TextStrong
	}
	body := func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, bg, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(7),
				Bottom: unit.Dp(7),
				Left:   unit.Dp(12),
				Right:  unit.Dp(12),
			}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				name := r.user.DisplayName
				if name == "" {
					name = r.user.Name
				}
				if name == "" {
					name = r.user.ID
				}
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						lbl := material.Body1(th.Mat, name)
						lbl.Color = color
						if active {
							lbl.Font.Weight = font.SemiBold
						}
						th.applyFont(&lbl, th.Fonts.Search)
						return lbl.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(10)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						hint := r.user.RealName
						if hint == "" || hint == name {
							hint = "@" + r.user.Name
						}
						lbl := material.Caption(th.Mat, hint)
						lbl.Color = th.Pal.TextDim
						th.applyFont(&lbl, FontStyle{})
						return lbl.Layout(gtx)
					}),
				)
			})
		})
	}
	return r.click.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		if active {
			return withBorder(gtx, th.Pal.Accent, borders{Left: true}, body)
		}
		return body(gtx)
	})
}
