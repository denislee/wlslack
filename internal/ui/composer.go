package ui

import (
	"strings"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// Composer is the bottom input row. Plain Enter sends the message; Shift+Enter
// inserts a newline (handled natively by the editor when Submit=true).
type Composer struct {
	editor widget.Editor
}

func newComposer() *Composer {
	c := &Composer{}
	c.editor.SingleLine = false
	c.editor.Submit = true
	return c
}

// Layout draws the composer. onSend is invoked with the (trimmed, non-empty)
// text whenever the user presses Enter.
func (c *Composer) Layout(gtx layout.Context, th *Theme, placeholder string, onSend func(string)) layout.Dimensions {
	for {
		ev, ok := c.editor.Update(gtx)
		if !ok {
			break
		}
		if _, isSubmit := ev.(widget.SubmitEvent); isSubmit {
			text := strings.TrimSpace(c.editor.Text())
			if text != "" {
				onSend(text)
			}
			c.editor.SetText("")
		}
	}

	return withBorder(gtx, th.Pal.Border, borders{Top: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.Pal.BgComposer, func(gtx layout.Context) layout.Dimensions {
			return layout.Inset{
				Top:    unit.Dp(10),
				Bottom: unit.Dp(10),
				Left:   unit.Dp(16),
				Right:  unit.Dp(16),
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
		})
	})
}

// Focus puts the keyboard focus on the editor.
func (c *Composer) Focus(gtx layout.Context) {
	gtx.Execute(key.FocusCmd{Tag: &c.editor})
}
