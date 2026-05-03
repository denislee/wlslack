package ui

import (
	"fmt"
	"image"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/user/wlslack/internal/slack"
)

// ImageViewer is the in-app image viewer that opens when the user presses
// Enter on a message that has image attachments and no links. It renders one
// image at a time scaled to fit, with a footer showing position and filename.
type ImageViewer struct {
	files   []slack.File
	idx     int
	images  *slack.ImageLoader
}

func newImageViewer(images *slack.ImageLoader) *ImageViewer {
	return &ImageViewer{images: images}
}

// SetFiles replaces the current set and resets the index. Pre-warms the loader
// for every file so neighbours decode while the user is still on the first.
func (v *ImageViewer) SetFiles(files []slack.File) {
	v.files = files
	v.idx = 0
	for _, f := range files {
		_, _, _ = v.images.GetOp(f.PreferredImageURL())
	}
}

// MoveSelection shifts the current image. Wraps at neither end — the user has
// to press Esc to leave the viewer.
func (v *ImageViewer) MoveSelection(delta int) {
	if len(v.files) == 0 {
		return
	}
	v.idx += delta
	if v.idx < 0 {
		v.idx = 0
	}
	if v.idx >= len(v.files) {
		v.idx = len(v.files) - 1
	}
}

// HasMultiple reports whether the user can flip between images.
func (v *ImageViewer) HasMultiple() bool { return len(v.files) > 1 }

// Layout draws the current image plus the footer.
func (v *ImageViewer) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if len(v.files) == 0 {
		return layout.Dimensions{}
	}
	cur := v.files[v.idx]
	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(8),
			Bottom: unit.Dp(8),
			Left:   unit.Dp(8),
			Right:  unit.Dp(8),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return v.layoutImage(gtx, th, cur)
					})
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return v.layoutFooter(gtx, th, cur)
				}),
			)
		})
	})
}

func (v *ImageViewer) layoutImage(gtx layout.Context, th *Theme, f slack.File) layout.Dimensions {
	op, hasOp, done := v.images.GetOp(f.PreferredImageURL())
	if !done {
		lbl := material.Body2(th.Mat, "loading "+f.Name+"…")
		lbl.Color = th.Pal.TextDim
		return lbl.Layout(gtx)
	}
	if !hasOp {
		lbl := material.Body2(th.Mat, f.Name+" (failed to load)")
		lbl.Color = th.Pal.TextDim
		return lbl.Layout(gtx)
	}
	sz := op.Size()
	if sz.X <= 0 || sz.Y <= 0 {
		return layout.Dimensions{}
	}
	maxPxW, maxPxH := gtx.Constraints.Max.X, gtx.Constraints.Max.Y
	if maxPxW <= 0 || maxPxH <= 0 {
		return layout.Dimensions{}
	}
	scale := float32(maxPxW) / float32(sz.X)
	if s := float32(maxPxH) / float32(sz.Y); s < scale {
		scale = s
	}
	target := image.Point{
		X: int(float32(sz.X) * scale),
		Y: int(float32(sz.Y) * scale),
	}
	if target.X < 1 || target.Y < 1 {
		return layout.Dimensions{}
	}
	gtx.Constraints = layout.Exact(target)
	w := widget.Image{
		Src:      op,
		Fit:      widget.Contain,
		Position: layout.Center,
	}
	return w.Layout(gtx)
}

func (v *ImageViewer) layoutFooter(gtx layout.Context, th *Theme, f slack.File) layout.Dimensions {
	hint := "Esc close"
	if v.HasMultiple() {
		hint = "j/k or ←/→ navigate · Esc close"
	}
	caption := fmt.Sprintf("%d / %d  %s", v.idx+1, len(v.files), f.Name)
	return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th.Mat, caption)
			lbl.Color = th.Pal.TextStrong
			lbl.Font.Weight = font.Medium
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Caption(th.Mat, hint)
			lbl.Color = th.Pal.TextMuted
			return lbl.Layout(gtx)
		}),
	)
}

