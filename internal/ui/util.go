package ui

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
)

// paintedBg fills the area occupied by w with bg, then renders w on top.
// We record w into a macro first, paint the background sized to its
// dimensions, then replay the recorded ops.
func paintedBg(gtx layout.Context, bg color.NRGBA, w layout.Widget) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	dims := w(gtx)
	call := macro.Stop()

	rect := clip.Rect{Max: dims.Size}.Push(gtx.Ops)
	paint.ColorOp{Color: bg}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	rect.Pop()

	call.Add(gtx.Ops)
	return dims
}

// drawRect fills the given size with c.
func drawRect(gtx layout.Context, c color.NRGBA, sz image.Point) {
	defer clip.Rect{Max: sz}.Push(gtx.Ops).Pop()
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// borders bundles which sides of a rectangle to stroke. Zero value = no border.
type borders struct {
	Top, Right, Bottom, Left bool
}

// withBorder runs w, then draws 1dp lines on the requested edges of its
// dimensions in c. Lines are drawn after w so they sit above any background
// or content. Static — no animation.
func withBorder(gtx layout.Context, c color.NRGBA, b borders, w layout.Widget) layout.Dimensions {
	dims := w(gtx)
	px := gtx.Dp(unit.Dp(1))
	if px < 1 {
		px = 1
	}
	sz := dims.Size
	stroke := func(r image.Rectangle) {
		if r.Dx() <= 0 || r.Dy() <= 0 {
			return
		}
		defer clip.Rect{Min: r.Min, Max: r.Max}.Push(gtx.Ops).Pop()
		paint.ColorOp{Color: c}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
	}
	if b.Top {
		stroke(image.Rect(0, 0, sz.X, px))
	}
	if b.Bottom {
		stroke(image.Rect(0, sz.Y-px, sz.X, sz.Y))
	}
	if b.Left {
		stroke(image.Rect(0, 0, px, sz.Y))
	}
	if b.Right {
		stroke(image.Rect(sz.X-px, 0, sz.X, sz.Y))
	}
	return dims
}

// WithAlpha returns a copy of c with its alpha component set to a.
func WithAlpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

// MoveWord jumps the editor caret to the start of the next (dir > 0) or
// previous (dir < 0) word.
func MoveWord(ed *widget.Editor, dir int) {
	runes := []rune(ed.Text())
	_, current := ed.Selection()
	if current > len(runes) {
		current = len(runes)
	}

	isSep := func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t'
	}

	if dir > 0 {
		foundSpace := false
		for i := current; i < len(runes); i++ {
			if isSep(runes[i]) {
				foundSpace = true
			} else if foundSpace {
				ed.SetCaret(i, i)
				return
			}
		}
		ed.SetCaret(len(runes), len(runes))
	} else {
		foundNonSpace := false
		for i := current - 1; i >= 0; i-- {
			if !isSep(runes[i]) {
				foundNonSpace = true
			} else if foundNonSpace {
				ed.SetCaret(i+1, i+1)
				return
			}
		}
		ed.SetCaret(0, 0)
	}
}

// MoveLine jumps the editor caret one line up (dir < 0) or down (dir > 0).
// It moves to the beginning of the target line. If already on the first/last
// line, it moves to the beginning/end of the text.
func MoveLine(ed *widget.Editor, dir int) {
	runes := []rune(ed.Text())
	_, current := ed.Selection()
	if current > len(runes) {
		current = len(runes)
	}

	// Calculate current column (rune offset from start of hard line)
	col := 0
	lineStart := 0
	for i := current - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			lineStart = i + 1
			break
		}
		col++
	}

	if dir < 0 {
		if lineStart == 0 {
			return // Already on first hard line
		}
		// Find start of previous hard line
		prevLineStart := 0
		for i := lineStart - 2; i >= 0; i-- {
			if runes[i] == '\n' {
				prevLineStart = i + 1
				break
			}
		}
		// Previous line end (the \n character itself)
		prevLineEnd := lineStart - 1

		target := prevLineStart + col
		if target > prevLineEnd {
			target = prevLineEnd
		}
		ed.SetCaret(target, target)
	} else {
		// Find end of current hard line
		lineEnd := len(runes)
		for i := current; i < len(runes); i++ {
			if runes[i] == '\n' {
				lineEnd = i
				break
			}
		}
		if lineEnd == len(runes) {
			return // Already on last hard line
		}

		nextLineStart := lineEnd + 1
		// Find end of next hard line
		nextLineEnd := len(runes)
		for i := nextLineStart; i < len(runes); i++ {
			if runes[i] == '\n' {
				nextLineEnd = i
				break
			}
		}

		target := nextLineStart + col
		if target > nextLineEnd {
			target = nextLineEnd
		}
		ed.SetCaret(target, target)
	}
}
