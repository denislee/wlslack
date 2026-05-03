package ui

import (
	"image"
	"io"
	"strings"

	"gioui.org/font"
	"gioui.org/io/clipboard"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

type editorMode int

const (
	modeNormal editorMode = iota
	modeVisual
)

// MessageEditor is a read-only full-screen editor for copying message parts.
type MessageEditor struct {
	editor   widget.Editor
	mode     editorMode
	vAnchor  int  // Selection anchor when entering Visual mode
	gPending bool // true if 'g' was pressed once
}

func newMessageEditor() *MessageEditor {
	e := &MessageEditor{}
	e.editor.ReadOnly = true
	e.editor.SingleLine = false
	return e
}

func (e *MessageEditor) SetText(text string) {
	e.editor.SetText(text)
	e.editor.SetCaret(0, 0)
	e.mode = modeNormal
	e.gPending = false
}

func (e *MessageEditor) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	// Drain IME-driven events targeted at the editor before its processKey
	// sees them. Without this, on Wayland every printable keypress fires a
	// key.EditEvent that Gio's window translates into EditorInsert, which
	// advances the editor's IME selection by 1 rune and dispatches a
	// key.SelectionEvent. widget.Editor applies that SelectionEvent
	// unconditionally — even when ReadOnly — moving the caret forward on
	// every keypress. Discarding these events here keeps our SetCaret-based
	// navigation authoritative. Draining FocusEvent doesn't unfocus the
	// editor (router state is unchanged), so paintSelection still works.
	for {
		_, ok := gtx.Source.Event(
			key.Filter{Focus: &e.editor},
			key.FocusFilter{Target: &e.editor},
		)
		if !ok {
			break
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
					modeStr := "NORMAL"
					if e.mode == modeVisual {
						modeStr = "VISUAL"
					}
					title := material.Subtitle1(th.Mat, "Message Editor ["+modeStr+"]")
					title.Color = th.Pal.TextStrong
					title.Font.Weight = font.SemiBold
					return title.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := "h/j/k/l navigate · v visual · y copy · gg/G top/bottom · Esc close"
					if e.mode == modeVisual {
						hint = "h/j/k/l select · y copy · Esc normal mode"
					}
					lbl := material.Caption(th.Mat, hint)
					lbl.Color = th.Pal.TextMuted
					return layout.Inset{Top: unit.Dp(4), Bottom: unit.Dp(12)}.Layout(gtx, lbl.Layout)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					macro := op.Record(gtx.Ops)
					ed := material.Editor(th.Mat, &e.editor, "")
					ed.Color = th.Pal.Text
					ed.SelectionColor = WithAlpha(th.Pal.Selection, 0x66)
					th.applyEditorFont(&ed, th.Fonts.Code)
					dims := ed.Layout(gtx)
					call := macro.Stop()

					call.Add(gtx.Ops)

					if gtx.Source.Focused(&e.editor) {
						e.drawCursor(gtx, th, dims)
					}

					return dims
				}),
			)
		})
	})
}

// HandleEsc returns true if it consumed the escape key (e.g. to exit visual mode).
func (e *MessageEditor) HandleEsc() bool {
	if e.mode == modeVisual {
		e.mode = modeNormal
		caret, _ := e.editor.Selection()
		e.editor.SetCaret(caret, caret)
		return true
	}
	return false
}

func (e *MessageEditor) drawCursor(gtx layout.Context, th *Theme, dims layout.Dimensions) {
	cpos := e.editor.CaretCoords()
	
	width := gtx.Dp(unit.Dp(8))
	color := WithAlpha(th.Pal.Accent, 0x80)
	if e.mode == modeVisual {
		width = gtx.Dp(unit.Dp(2))
		color = th.Pal.Accent
	}

	size := th.Mat.TextSize
	if th.Fonts.Code.Size > 0 {
		size = unit.Sp(th.Fonts.Code.Size)
	}
	height := gtx.Sp(size)

	// CaretCoords returns the baseline position, not the line top. Shift up
	// by the approximate ascent (~80% of em) so the block covers the glyph.
	ascent := (height * 4) / 5
	top := int(cpos.Y) - ascent
	rect := image.Rect(int(cpos.X), top, int(cpos.X)+width, top+height)

	defer clip.Rect{Max: dims.Size}.Push(gtx.Ops).Pop()
	defer clip.Rect{Min: rect.Min, Max: rect.Max}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: color}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// KeyFilters returns the key filters this editor wants routed to it via
// HandleKey when it is open. Registering them at the App level ensures the
// events are actually delivered (the filter syntax requires explicit Name).
// Arrow keys are filtered with Focus on the editor so we drain them before
// widget.Editor.processKey can move the caret on its own.
func (e *MessageEditor) KeyFilters() []event.Filter {
	t := event.Tag(&e.editor)
	return []event.Filter{
		key.Filter{Focus: t, Name: "H"},
		key.Filter{Focus: t, Name: "J"},
		key.Filter{Focus: t, Name: "K"},
		key.Filter{Focus: t, Name: "L"},
		key.Filter{Focus: t, Name: "V"},
		key.Filter{Focus: t, Name: "Y"},
		key.Filter{Focus: t, Name: "W"},
		key.Filter{Focus: t, Name: "B"},
		key.Filter{Focus: t, Name: "G", Optional: key.ModShift},
		key.Filter{Focus: t, Name: "0"},
		// On Wayland/X11 (XKB), Shift+4 arrives as Name="$" with ModShift
		// still set (the keysym reflects the shifted symbol but the modifier
		// state is reported as-held). On Windows it arrives as Name="4" with
		// ModShift. Accept both, treating Shift as optional.
		key.Filter{Focus: t, Name: "$", Optional: key.ModShift},
		key.Filter{Focus: t, Name: "4", Required: key.ModShift},
		key.Filter{Focus: t, Name: key.NameLeftArrow},
		key.Filter{Focus: t, Name: key.NameRightArrow},
		key.Filter{Focus: t, Name: key.NameUpArrow},
		key.Filter{Focus: t, Name: key.NameDownArrow},
		key.Filter{Focus: t, Name: key.NameHome},
		key.Filter{Focus: t, Name: key.NameEnd},
	}
}

// HandleKey processes a single key.Event. Returns true if the event was
// recognized and consumed.
func (e *MessageEditor) HandleKey(gtx layout.Context, ev key.Event) bool {
	if ev.State != key.Press {
		return false
	}
	name := ev.Name
	if name == "4" && ev.Modifiers.Contain(key.ModShift) {
		name = "$"
	}
	switch name {
	case "V":
		if e.mode == modeNormal {
			e.mode = modeVisual
			caret, _ := e.editor.Selection()
			e.vAnchor = caret
		} else {
			e.mode = modeNormal
			caret, _ := e.editor.Selection()
			e.editor.SetCaret(caret, caret)
		}
		e.gPending = false
	case "Y":
		e.copySelected(gtx)
		if e.mode == modeVisual {
			e.mode = modeNormal
			caret, _ := e.editor.Selection()
			e.editor.SetCaret(caret, caret)
		}
		e.gPending = false
	case "H", key.NameLeftArrow:
		e.moveCaretRelative(-1)
		e.gPending = false
	case "L", key.NameRightArrow:
		e.moveCaretRelative(1)
		e.gPending = false
	case "J", key.NameDownArrow:
		e.moveLine(1)
		e.gPending = false
	case "K", key.NameUpArrow:
		e.moveLine(-1)
		e.gPending = false
	case "W":
		e.moveWord(1)
		e.gPending = false
	case "B":
		e.moveWord(-1)
		e.gPending = false
	case "0", key.NameHome:
		e.moveLineBoundary(-1)
		e.gPending = false
	case "$", key.NameEnd:
		e.moveLineBoundary(1)
		e.gPending = false
	case "G":
		if ev.Modifiers.Contain(key.ModShift) {
			e.moveCaret(e.editor.Len())
			e.gPending = false
		} else if e.gPending {
			e.moveCaret(0)
			e.gPending = false
		} else {
			e.gPending = true
		}
	default:
		return false
	}
	return true
}

// runes returns the editor text as a rune slice. Selection()/SetCaret() use
// rune offsets, so all navigation must work in rune space — indexing the
// string by byte drifts with every multi-byte character (emoji, accents).
func (e *MessageEditor) runes() []rune {
	return []rune(e.editor.Text())
}

func (e *MessageEditor) moveCaret(pos int) {
	n := len(e.runes())
	if pos < 0 {
		pos = 0
	}
	if pos > n {
		pos = n
	}
	if e.mode == modeVisual {
		e.editor.SetCaret(e.vAnchor, pos)
	} else {
		e.editor.SetCaret(pos, pos)
	}
}

func (e *MessageEditor) moveCaretRelative(delta int) {
	_, end := e.editor.Selection()
	e.moveCaret(end + delta)
}

func (e *MessageEditor) moveLine(dir int) {
	rs := e.runes()
	_, current := e.editor.Selection()
	if current > len(rs) {
		current = len(rs)
	}

	if dir > 0 {
		for i := current; i < len(rs); i++ {
			if rs[i] == '\n' {
				e.moveCaret(i + 1)
				return
			}
		}
		e.moveCaret(len(rs))
	} else {
		if current == 0 {
			return
		}
		lineStart := 0
		for i := current - 1; i >= 0; i-- {
			if rs[i] == '\n' {
				lineStart = i + 1
				break
			}
		}
		if lineStart == 0 {
			e.moveCaret(0)
			return
		}
		prevLineStart := 0
		for i := lineStart - 2; i >= 0; i-- {
			if rs[i] == '\n' {
				prevLineStart = i + 1
				break
			}
		}
		e.moveCaret(prevLineStart)
	}
}

func (e *MessageEditor) moveWord(dir int) {
	rs := e.runes()
	_, current := e.editor.Selection()
	if current > len(rs) {
		current = len(rs)
	}

	isSep := func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }

	if dir > 0 {
		foundSpace := false
		for i := current; i < len(rs); i++ {
			if isSep(rs[i]) {
				foundSpace = true
			} else if foundSpace {
				e.moveCaret(i)
				return
			}
		}
		e.moveCaret(len(rs))
	} else {
		foundNonSpace := false
		for i := current - 1; i >= 0; i-- {
			if !isSep(rs[i]) {
				foundNonSpace = true
			} else if foundNonSpace {
				e.moveCaret(i + 1)
				return
			}
		}
		e.moveCaret(0)
	}
}

func (e *MessageEditor) moveLineBoundary(dir int) {
	rs := e.runes()
	_, current := e.editor.Selection()
	if current > len(rs) {
		current = len(rs)
	}

	if dir < 0 {
		for i := current - 1; i >= 0; i-- {
			if rs[i] == '\n' {
				e.moveCaret(i + 1)
				return
			}
		}
		e.moveCaret(0)
	} else {
		// Land on the last visible character of the line so the block
		// cursor sits on it (vim-style $). Position-of-\n would place
		// the cursor past the line in Gio's text layout.
		for i := current; i < len(rs); i++ {
			if rs[i] == '\n' {
				if i > current {
					e.moveCaret(i - 1)
				} else {
					e.moveCaret(i)
				}
				return
			}
		}
		if len(rs) > current {
			e.moveCaret(len(rs) - 1)
		} else {
			e.moveCaret(len(rs))
		}
	}
}

func (e *MessageEditor) copySelected(gtx layout.Context) {
	text := e.editor.SelectedText()
	if text == "" {
		text = e.editor.Text()
	}
	if text != "" {
		gtx.Execute(clipboard.WriteCmd{
			Type: "application/text",
			Data: io.NopCloser(strings.NewReader(text)),
		})
	}
}

func (e *MessageEditor) Focus(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &e.editor})
}

// FocusTag returns the focus tag this editor uses for key events. Use this
// instead of the underlying widget.Editor when registering key.Filters.
func (e *MessageEditor) FocusTag() event.Tag {
	return &e.editor
}
