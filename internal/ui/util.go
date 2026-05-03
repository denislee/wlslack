package ui

import (
	"image"
	"image/color"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
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
