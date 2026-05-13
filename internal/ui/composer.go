package ui

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"image/color"
	"strings"
	"sync"

	"gioui.org/font"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

type Attachment struct {
	Name  string
	Data  []byte
	Image image.Image
}

type VimMode int

const (
	VimInsert VimMode = iota
	VimNormal
	VimVisual
)

// Composer is the bottom input row.
type Composer struct {
	editor             widget.Editor
	mentionPicker      *MentionPicker
	attachBtn          widget.Clickable
	pendingAttachments []Attachment
	removeBtns         []widget.Clickable

	vimMode      VimMode
	visualAnchor int

	// history stores sent messages. historyIdx is the current position in
	// history. -1 means we are at the end (the current draft).
	history      []string
	historyIdx   int
	currentDraft string

	// Tracks if the last Up/Down press hit a boundary without moving.
	atUpBoundary   bool
	atDownBoundary bool

	justSwitched bool

	pendingOp rune

	// origText is set on the first Ctrl+T press so a second press can swap
	// the translated text back to what the user typed. Empty means no
	// translation is currently active.
	origText string

	pendingMu     sync.Mutex
	pendingResult string
	pendingSet    bool
}

func newComposer() *Composer {
	c := &Composer{
		mentionPicker: newMentionPicker(),
		historyIdx:    -1,
		vimMode:       VimInsert,
	}
	c.editor.SingleLine = false
	c.editor.Submit = false // We handle Ctrl-Enter manually
	return c
}

// Layout draws the composer. onSend is invoked with the (trimmed, non-empty)
// text whenever the user presses Enter.
func (c *Composer) Layout(gtx layout.Context, th *Theme, fm *slack.Formatter, placeholder string, onSend func(string, []Attachment), onAttach func()) layout.Dimensions {
	c.pendingMu.Lock()
	if c.pendingSet {
		c.editor.SetText(c.pendingResult)
		c.pendingResult = ""
		c.pendingSet = false
	}
	c.pendingMu.Unlock()

	if c.attachBtn.Clicked(gtx) {
		onAttach()
	}

	for i := 0; i < len(c.removeBtns); i++ {
		if c.removeBtns[i].Clicked(gtx) {
			c.pendingAttachments = append(c.pendingAttachments[:i], c.pendingAttachments[i+1:]...)
			c.removeBtns = append(c.removeBtns[:i], c.removeBtns[i+1:]...)
			i--
		}
	}

	c.updateMentions(fm)

	oldText := c.editor.Text()
	oldStart, oldEnd := c.editor.Selection()

	for {
		ev, ok := c.editor.Update(gtx)
		if !ok {
			break
		}
		if _, isSubmit := ev.(widget.SubmitEvent); isSubmit {
			c.Submit(onSend)
		}
	}

	if (c.vimMode == VimNormal || c.vimMode == VimVisual || c.justSwitched) && c.editor.Text() != oldText {
		c.editor.SetText(oldText)
		c.editor.SetCaret(oldStart, oldEnd)
	}
	c.justSwitched = false

	return withBorder(gtx, th.Pal.Border, borders{Top: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.Pal.BgComposer, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if len(c.pendingAttachments) == 0 {
						return layout.Dimensions{}
					}
					return layout.Inset{Top: unit.Dp(8), Left: unit.Dp(16), Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Flex{}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								list := layout.List{Axis: layout.Horizontal}
								return list.Layout(gtx, len(c.pendingAttachments), func(gtx layout.Context, i int) layout.Dimensions {
									return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										return withBorder(gtx, th.Pal.BorderStrong, borders{Top: true, Right: true, Bottom: true, Left: true}, func(gtx layout.Context) layout.Dimensions {
											return layout.Inset{Left: unit.Dp(8), Right: unit.Dp(4), Top: unit.Dp(2), Bottom: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
												return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
													layout.Rigid(func(gtx layout.Context) layout.Dimensions {
														if c.pendingAttachments[i].Image == nil {
															return layout.Dimensions{}
														}
														return layout.Inset{Right: unit.Dp(4)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
															gtx.Constraints.Max.Y = gtx.Dp(unit.Dp(64))
															gtx.Constraints.Max.X = gtx.Dp(unit.Dp(64))
															m := paint.NewImageOp(c.pendingAttachments[i].Image)
															img := widget.Image{Src: m, Fit: widget.Contain}
															return img.Layout(gtx)
														})
													}),
													layout.Rigid(func(gtx layout.Context) layout.Dimensions {
														lbl := material.Caption(th.Mat, c.pendingAttachments[i].Name)
														lbl.Color = th.Pal.Text
														return lbl.Layout(gtx)
													}),
													layout.Rigid(func(gtx layout.Context) layout.Dimensions {
														btn := material.Button(th.Mat, &c.removeBtns[i], "x")
														btn.Background = color.NRGBA{}
														btn.Color = th.Pal.TextDim
														btn.Inset = layout.UniformInset(unit.Dp(2))
														btn.TextSize = unit.Sp(14)
														return btn.Layout(gtx)
													}),
												)
											})
										})
									})
								})
							}),
						)
					})
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Stack{}.Layout(gtx,
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{
										Top:    unit.Dp(10),
										Bottom: unit.Dp(10),
										Left:   unit.Dp(16),
										Right:  unit.Dp(8),
									}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										ed := material.Editor(th.Mat, &c.editor, placeholder)
										ed.Color = th.Pal.Text
										ed.HintColor = th.Pal.TextMuted
										ed.SelectionColor = WithAlpha(th.Pal.Selection, 0x66)
										if th.Fonts.Composer.Face != "" {
											ed.Font.Typeface = font.Typeface(th.Fonts.Composer.Face)
										}
										if th.Fonts.Composer.Size > 0 {
											ed.TextSize = unit.Sp(th.Fonts.Composer.Size)
										}
										return ed.Layout(gtx)
									})
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Inset{Right: unit.Dp(16)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
										return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												modeText := "INSERT"
												modeColor := th.Pal.Accent
												switch c.vimMode {
												case VimNormal:
													modeText = "NORMAL"
													modeColor = th.Pal.TextDim
												case VimVisual:
													modeText = "VISUAL"
													modeColor = th.Pal.Accent
												}
												lbl := material.Caption(th.Mat, modeText)
												lbl.Color = modeColor
												lbl.Font.Weight = font.Bold
												return layout.Inset{Right: unit.Dp(8)}.Layout(gtx, lbl.Layout)
											}),
											layout.Rigid(func(gtx layout.Context) layout.Dimensions {
												btn := material.Button(th.Mat, &c.attachBtn, "+")
												btn.Background = color.NRGBA{} // Transparent
												btn.Color = th.Pal.TextDim
												btn.Inset = layout.UniformInset(unit.Dp(8))
												if c.attachBtn.Hovered() {
													btn.Color = th.Pal.Accent
												}
												// Force a bit of size so it's clickable
												gtx.Constraints.Min.X = gtx.Dp(unit.Dp(24))
												gtx.Constraints.Min.Y = gtx.Dp(unit.Dp(24))
												return btn.Layout(gtx)
											}),
										)
									})
								}),
							)
						}),
						layout.Stacked(func(gtx layout.Context) layout.Dimensions {
							if !c.mentionPicker.Active() {
								return layout.Dimensions{}
							}
							// Draw picker above the composer
							return layout.Inset{Bottom: unit.Dp(45)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
								return c.mentionPicker.Layout(gtx, th)
							})
						}),
					)
				}),
			)
		})
	})
}

func (c *Composer) Mode() VimMode {
	return c.vimMode
}

func (c *Composer) SetInsertMode() {
	c.vimMode = VimInsert
	c.pendingOp = 0
	c.justSwitched = true
}

func (c *Composer) KeyFilters() []event.Filter {
	f := []event.Filter{
		key.Filter{Focus: &c.editor, Name: key.NameEscape},
		key.Filter{Focus: &c.editor, Name: "[", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: key.NameReturn, Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "W", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: key.NameDeleteBackward, Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "T", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "A", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "E", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "F", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "B", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "C", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "V", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "v", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "P", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: "N", Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: key.NameLeftArrow, Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: key.NameRightArrow, Required: key.ModCtrl},
		key.Filter{Focus: &c.editor, Name: key.NameUpArrow},
		key.Filter{Focus: &c.editor, Name: key.NameDownArrow},
		key.Filter{Focus: &c.editor, Name: "$", Required: key.ModShift},
	}

	if c.vimMode == VimNormal || c.vimMode == VimVisual {
		// In Normal/Visual mode, we filter all key events to prevent them from
		// reaching the editor and changing the text.
		f = append(f, key.Filter{Focus: &c.editor})
	}

	if c.mentionPicker.Active() {
		f = append(f,
			key.Filter{Focus: &c.editor, Name: key.NameReturn},
			key.Filter{Focus: &c.editor, Name: key.NameTab},
			key.Filter{Focus: &c.editor, Name: "Y", Required: key.ModCtrl},
		)
	}
	return f
}

func (c *Composer) HandleKey(gtx layout.Context, kev key.Event, onSend func(string, []Attachment)) bool {
	if kev.Name == key.NameReturn && (kev.Modifiers.Contain(key.ModCtrl) || (c.mentionPicker.Active())) {
		if c.mentionPicker.Active() {
			c.mentionPicker.Submit()
		} else {
			c.Submit(onSend)
		}
		return true
	}

	if kev.Name == key.NameTab && c.mentionPicker.Active() {
		c.mentionPicker.Submit()
		return true
	}

	if c.mentionPicker.Active() && kev.Name == "Y" && kev.Modifiers.Contain(key.ModCtrl) {
		c.mentionPicker.Submit()
		return true
	}

	if c.vimMode == VimNormal {
		handled := true

		if c.pendingOp != 0 {
			op := c.pendingOp
			c.pendingOp = 0
			switch op {
			case 'd':
				switch kev.Name {
				case "D":
					if !kev.Modifiers.Contain(key.ModShift) {
						c.DeleteLine()
					}
				case "W":
					c.DeleteWord(1)
				case "B":
					c.DeleteWord(-1)
				case "L":
					c.DeleteChar()
				case "H":
					c.MoveCursor(-1)
					c.DeleteChar()
				case "0":
					start, _ := c.editor.Selection()
					c.MoveToLineStart()
					_, end := c.editor.Selection()
					if start > end {
						start, end = end, start
					}
					c.editor.SetCaret(start, end)
					c.editor.Insert("")
				case "$":
					c.DeleteToEndOfLine()
				}
				return true
			}
		}

		switch kev.Name {
		case "I":
			c.vimMode = VimInsert
			c.justSwitched = true
		case "A":
			c.MoveCursor(1)
			c.vimMode = VimInsert
			c.justSwitched = true
		case "H":
			c.MoveCursor(-1)
		case "L":
			c.MoveCursor(1)
		case "J":
			c.MoveLine(1)
		case "K":
			c.MoveLine(-1)
		case "X":
			c.DeleteChar()
		case "0":
			c.MoveToLineStart()
		case "$":
			c.MoveToLineEnd()
		case "W":
			c.MoveWord(1)
		case "B":
			c.MoveWord(-1)
		case "G":
			if kev.Modifiers.Contain(key.ModShift) {
				c.MoveToEnd()
			} else {
				c.MoveToStart()
			}
		case "D":
			if kev.Modifiers.Contain(key.ModShift) {
				c.DeleteToEndOfLine()
			} else {
				c.pendingOp = 'd'
			}
		case "P":
			c.HistoryPrev()
		case "N":
			c.HistoryNext()
		case "V":
			if kev.Modifiers.Contain(key.ModCtrl) {
				handled = false
			} else if !kev.Modifiers.Contain(key.ModShift) {
				c.EnterVisual()
			}
		case "T":
			if kev.Modifiers.Contain(key.ModCtrl) {
				handled = false
			}
		case key.NameEscape:
			// Returning false here allows the app-level handler to catch Esc and exit focus
			handled = false
		case "[":
			if kev.Modifiers.Contain(key.ModCtrl) {
				handled = false
			}
		default:
			// Discard other keys in Normal mode
		}
		return handled
	} else if c.vimMode == VimVisual {
		handled := true
		switch kev.Name {
		case "H":
			c.MoveVisual(func() { c.MoveCursor(-1) })
		case "L":
			c.MoveVisual(func() { c.MoveCursor(1) })
		case "J":
			c.MoveVisual(func() { c.MoveLine(1) })
		case "K":
			c.MoveVisual(func() { c.MoveLine(-1) })
		case "0":
			c.MoveVisual(c.MoveToLineStart)
		case "$":
			c.MoveVisual(c.MoveToLineEnd)
		case "W":
			c.MoveVisual(func() { c.MoveWord(1) })
		case "B":
			c.MoveVisual(func() { c.MoveWord(-1) })
		case "G":
			if kev.Modifiers.Contain(key.ModShift) {
				c.MoveVisual(c.MoveToEnd)
			} else {
				c.MoveVisual(c.MoveToStart)
			}
		case "Y":
			c.VisualYank(gtx)
			c.ExitVisual()
		case "D", "X":
			c.VisualDelete()
			c.ExitVisual()
		case "C":
			c.VisualDelete()
			c.vimMode = VimInsert
			c.justSwitched = true
		case "V":
			c.ExitVisual()
		case key.NameEscape:
			c.ExitVisual()
		case "[":
			if kev.Modifiers.Contain(key.ModCtrl) {
				c.ExitVisual()
			}
		default:
			// Discard other keys in Visual mode
		}
		return handled
	} else {
		if kev.Name == key.NameEscape || (kev.Name == "[" && kev.Modifiers.Contain(key.ModCtrl)) {
			c.vimMode = VimNormal
			return true
		}

		// Standard shortcuts in Insert mode
		if kev.Modifiers.Contain(key.ModCtrl) {
			switch kev.Name {
			case "W", key.NameDeleteBackward:
				c.DeleteLastWord()
				return true
			case "A":
				c.SelectAll()
				return true
			case "E":
				c.MoveToEnd()
				return true
			case "F":
				c.MoveCursor(1)
				return true
			case "B":
				c.MoveCursor(-1)
				return true
			case "C":
				c.Clear()
				return true
			case "P":
				if c.mentionPicker.Active() {
					c.mentionPicker.MoveSelection(-1)
				} else {
					c.HistoryPrev()
				}
				return true
			case "N":
				if c.mentionPicker.Active() {
					c.mentionPicker.MoveSelection(1)
				} else {
					c.HistoryNext()
				}
				return true
			case "T", "V":
				return false
			case key.NameLeftArrow:
				c.MoveWord(-1)
				return true
			case key.NameRightArrow:
				c.MoveWord(1)
				return true
			}
		}

		// History and mention navigation with arrows
		if kev.Name == key.NameUpArrow {
			if c.mentionPicker.Active() {
				c.mentionPicker.MoveSelection(-1)
				c.atUpBoundary = false
				c.atDownBoundary = false
			} else {
				oldPos, _ := c.editor.Selection()
				c.MoveLine(-1)
				newPos, _ := c.editor.Selection()
				if oldPos == newPos {
					if c.atUpBoundary {
						if oldPos == 0 {
							c.HistoryPrev()
						} else {
							c.MoveToStart()
						}
					} else {
						c.atUpBoundary = true
					}
				} else {
					c.atUpBoundary = false
				}
				c.atDownBoundary = false
			}
			return true
		}
		if kev.Name == key.NameDownArrow {
			if c.mentionPicker.Active() {
				c.mentionPicker.MoveSelection(1)
				c.atUpBoundary = false
				c.atDownBoundary = false
			} else {
				oldPos, _ := c.editor.Selection()
				c.MoveLine(1)
				newPos, _ := c.editor.Selection()
				if oldPos == newPos {
					lastPos := len([]rune(c.editor.Text()))
					if c.atDownBoundary {
						if oldPos == lastPos {
							c.HistoryNext()
						} else {
							c.MoveToEnd()
						}
					} else {
						c.atDownBoundary = true
					}
				} else {
					c.atDownBoundary = false
				}
				c.atUpBoundary = false
			}
			return true
		}
	}

	return false
}

func (c *Composer) DeleteChar() {
	_, end := c.editor.Selection()
	runes := []rune(c.editor.Text())
	if end >= len(runes) {
		return
	}
	c.editor.SetCaret(end, end+1)
	c.editor.Insert("")
}

func (c *Composer) DeleteToEndOfLine() {
	start, _ := c.editor.Selection()
	c.MoveToLineEnd()
	_, end := c.editor.Selection()
	c.editor.SetCaret(start, end)
	c.editor.Insert("")
}

func (c *Composer) DeleteLine() {
	c.MoveToLineStart()
	start, _ := c.editor.Selection()
	c.MoveToLineEnd()
	_, end := c.editor.Selection()

	runes := []rune(c.editor.Text())
	if end < len(runes) && runes[end] == '\n' {
		end++
	} else if start > 0 && runes[start-1] == '\n' {
		start--
	}

	c.editor.SetCaret(start, end)
	c.editor.Insert("")
}

func (c *Composer) DeleteWord(dir int) {
	start, _ := c.editor.Selection()
	MoveWord(&c.editor, dir)
	_, end := c.editor.Selection()
	if start > end {
		start, end = end, start
	}
	c.editor.SetCaret(start, end)
	c.editor.Insert("")
}

func (c *Composer) Submit(onSend func(string, []Attachment)) {
	text := strings.TrimSpace(c.editor.Text())
	if text != "" || len(c.pendingAttachments) > 0 {
		onSend(text, c.pendingAttachments)
		if text != "" {
			c.history = append(c.history, text)
		}
	}
	c.editor.SetText("")
	c.origText = ""
	c.historyIdx = -1
	c.currentDraft = ""
	c.mentionPicker.Close()
	c.ClearAttachments()
	c.vimMode = VimInsert // Back to insert mode after send?
}

func (c *Composer) updateMentions(fm *slack.Formatter) {
	caret, _ := c.editor.Selection()
	text := c.editor.Text()
	runes := []rune(text)
	if caret > len(runes) {
		caret = len(runes)
	}

	// Look for '@' before the caret
	start := caret - 1
	for start >= 0 {
		if runes[start] == '@' {
			// Found '@', check if it's the start of a word or start of text
			if start == 0 || runes[start-1] == ' ' || runes[start-1] == '\n' {
				query := string(runes[start+1 : caret])
				if !strings.Contains(query, " ") {
					c.mentionPicker.Open(query, fm, func(id, label string, isGroup bool) {
						// Replace @query with mention
						prefix := string(runes[:start])
						suffix := string(runes[caret:])
						mention := "<@" + id + ">"
						if isGroup {
							mention = "<!subteam^" + id + ">"
						} else if id == "here" || id == "channel" || id == "everyone" {
							mention = "<!" + id + ">"
						}
						c.editor.SetText(prefix + mention + " " + suffix)
						newPos := len([]rune(prefix + mention + " "))
						c.editor.SetCaret(newPos, newPos)
						c.mentionPicker.Close()
					})
					return
				}
			}
			break
		}
		if runes[start] == ' ' || runes[start] == '\n' {
			break
		}
		start--
	}
	c.mentionPicker.Close()
}

// Focus puts the keyboard focus on the editor.
func (c *Composer) Focus(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &c.editor})
}

func (c *Composer) DeleteLastWord() {
	_, end := c.editor.Selection()
	if end == 0 {
		return
	}

	runes := []rune(c.editor.Text())
	if end > len(runes) {
		end = len(runes)
	}

	isSep := func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t'
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
		c.editor.SetCaret(pos, end)
		c.editor.Insert("")
	}
}

func (c *Composer) MoveToLineStart() {
	runes := []rune(c.editor.Text())
	_, current := c.editor.Selection()
	if current > len(runes) {
		current = len(runes)
	}
	start := 0
	for i := current - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			start = i + 1
			break
		}
	}
	c.editor.SetCaret(start, start)
}

func (c *Composer) MoveToLineEnd() {
	runes := []rune(c.editor.Text())
	_, current := c.editor.Selection()
	if current > len(runes) {
		current = len(runes)
	}
	end := len(runes)
	for i := current; i < len(runes); i++ {
		if runes[i] == '\n' {
			end = i
			break
		}
	}
	c.editor.SetCaret(end, end)
}

func (c *Composer) MoveToStart() {
	c.editor.SetCaret(0, 0)
}

func (c *Composer) SelectAll() {
	n := len([]rune(c.editor.Text()))
	c.editor.SetCaret(0, n)
}

func (c *Composer) MoveToEnd() {
	n := len([]rune(c.editor.Text()))
	c.editor.SetCaret(n, n)
}

// EnterVisual transitions from Normal to Visual mode, anchoring the selection
// at the current caret position.
func (c *Composer) EnterVisual() {
	cursor, _ := c.editor.Selection()
	c.visualAnchor = cursor
	c.vimMode = VimVisual
	c.justSwitched = true
	c.editor.SetCaret(cursor, cursor)
}

// ExitVisual returns to Normal mode and collapses the selection to the caret.
func (c *Composer) ExitVisual() {
	cursor, _ := c.editor.Selection()
	c.editor.SetCaret(cursor, cursor)
	c.vimMode = VimNormal
}

// MoveVisual runs an existing movement helper while preserving the visual
// selection anchor. Movement helpers expect a collapsed selection, so we
// collapse to the active caret end before moving and re-extend after.
func (c *Composer) MoveVisual(move func()) {
	cursor, _ := c.editor.Selection()
	c.editor.SetCaret(cursor, cursor)
	move()
	newCursor, _ := c.editor.Selection()
	c.editor.SetCaret(newCursor, c.visualAnchor)
}

func (c *Composer) visualBounds() (int, int) {
	cursor, anchor := c.editor.Selection()
	lo, hi := cursor, anchor
	if lo > hi {
		lo, hi = hi, lo
	}
	// Inclusive selection on the right end, mirroring vim char-wise visual.
	runes := []rune(c.editor.Text())
	if hi < len(runes) {
		hi++
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(runes) {
		hi = len(runes)
	}
	return lo, hi
}

// VisualYank copies the visually-selected text to the system clipboard.
func (c *Composer) VisualYank(gtx layout.Context) {
	lo, hi := c.visualBounds()
	if lo >= hi {
		return
	}
	runes := []rune(c.editor.Text())
	text := string(runes[lo:hi])
	writeClipboardText(gtx, text)
}

// VisualDelete removes the visually-selected text from the editor.
func (c *Composer) VisualDelete() {
	lo, hi := c.visualBounds()
	if lo >= hi {
		return
	}
	c.editor.SetCaret(lo, hi)
	c.editor.Insert("")
}

func (c *Composer) MoveCursor(delta int) {
	_, end := c.editor.Selection()
	n := len([]rune(c.editor.Text()))
	newPos := end + delta
	if newPos < 0 {
		newPos = 0
	}
	if newPos > n {
		newPos = n
	}
	c.editor.SetCaret(newPos, newPos)
}

func (c *Composer) MoveWord(dir int) {
	MoveWord(&c.editor, dir)
}

func (c *Composer) MoveLine(dir int) {
	MoveLine(&c.editor, dir)
}

func (c *Composer) Clear() {
	c.editor.SetText("")
	c.origText = ""
	c.ClearAttachments()
}

func (c *Composer) AddAttachment(name string, data []byte) {
	img, _, _ := image.Decode(bytes.NewReader(data))
	c.pendingAttachments = append(c.pendingAttachments, Attachment{Name: name, Data: data, Image: img})
	c.removeBtns = append(c.removeBtns, widget.Clickable{})
}

func (c *Composer) ClearAttachments() {
	c.pendingAttachments = nil
	c.removeBtns = nil
}

func (c *Composer) SetPendingText(text string) {
	c.pendingMu.Lock()
	c.pendingResult = text
	c.pendingSet = true
	c.pendingMu.Unlock()
}

func (c *Composer) HistoryPrev() {
	if c.historyIdx == -2 {
		c.historyIdx = -1
		c.editor.SetText(c.currentDraft)
		c.MoveToEnd()
		return
	}
	if len(c.history) == 0 {
		return
	}
	if c.historyIdx == -1 {
		c.currentDraft = c.editor.Text()
		c.historyIdx = len(c.history) - 1
	} else if c.historyIdx > 0 {
		c.historyIdx--
	} else {
		return
	}
	c.editor.SetText(c.history[c.historyIdx])
	c.MoveToEnd()
}

func (c *Composer) HistoryNext() {
	if c.historyIdx == -2 {
		return
	}
	if c.historyIdx == -1 {
		c.currentDraft = c.editor.Text()
		c.historyIdx = -2
		c.editor.SetText("")
		c.MoveToEnd()
		return
	}
	if c.historyIdx < len(c.history)-1 {
		c.historyIdx++
		c.editor.SetText(c.history[c.historyIdx])
	} else {
		c.historyIdx = -1
		c.editor.SetText(c.currentDraft)
	}
	c.MoveToEnd()
}

// TranslateToEnglish toggles between the user's text and an English
// translation. On the first call it captures the current text and invokes
// onTranslate, which is expected to run the network call asynchronously and
// then call done with the result. On a subsequent call (while a translation
// is active) it swaps the editor back to the original text.
//
// The done callback is safe to call from any goroutine; the result is
// applied to the editor inside Layout on the UI goroutine.
func (c *Composer) TranslateToEnglish(onTranslate func(text string, setFeedback func(string), done func(translated string, err error))) {
	if c.origText != "" {
		c.editor.SetText(c.origText)
		c.origText = ""
		return
	}
	text := c.editor.Text()
	if strings.TrimSpace(text) == "" {
		return
	}
	c.origText = text
	onTranslate(text, func(feedback string) {
		c.editor.SetText(feedback)
	}, func(translated string, err error) {
		if err != nil {
			c.pendingMu.Lock()
			c.pendingSet = false
			c.pendingResult = ""
			c.pendingMu.Unlock()
			c.origText = ""
			return
		}
		c.pendingMu.Lock()
		c.pendingResult = translated
		c.pendingSet = true
		c.pendingMu.Unlock()
	})
}
