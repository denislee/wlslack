package ui

import (
	"strings"
	"sync"

	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// Composer is the bottom input row. Plain Enter sends the message; Shift+Enter
// inserts a newline (handled natively by the editor when Submit=true).
type Composer struct {
	editor        widget.Editor
	mentionPicker *MentionPicker

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
	}
	c.editor.SingleLine = false
	c.editor.Submit = true
	return c
}

// Layout draws the composer. onSend is invoked with the (trimmed, non-empty)
// text whenever the user presses Enter.
func (c *Composer) Layout(gtx layout.Context, th *Theme, fm *slack.Formatter, placeholder string, onSend func(string)) layout.Dimensions {
	c.pendingMu.Lock()
	if c.pendingSet {
		c.editor.SetText(c.pendingResult)
		c.pendingResult = ""
		c.pendingSet = false
	}
	c.pendingMu.Unlock()

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
			c.origText = ""
			c.mentionPicker.Close()
		}
	}

	c.updateMentions(fm)

	return withBorder(gtx, th.Pal.Border, borders{Top: true}, func(gtx layout.Context) layout.Dimensions {
		return paintedBg(gtx, th.Pal.BgComposer, func(gtx layout.Context) layout.Dimensions {
			return layout.Stack{}.Layout(gtx,
				layout.Stacked(func(gtx layout.Context) layout.Dimensions {
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
		})
	})
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

func (c *Composer) MoveToStart() {
	c.editor.SetCaret(0, 0)
}

func (c *Composer) MoveToEnd() {
	n := len([]rune(c.editor.Text()))
	c.editor.SetCaret(n, n)
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

func (c *Composer) Clear() {
	c.editor.SetText("")
	c.origText = ""
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
