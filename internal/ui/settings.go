package ui

import (
	"fmt"

	"gioui.org/font"
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// SettingsScreen is the per-section font configuration overlay. It mutates
// th.Fonts in place and fires onChange whenever the user picks a different
// face or size, so the host can persist to disk.
type SettingsScreen struct {
	th       *Theme
	onChange func()
	onClose  func()

	rows  []*settingsRow
	close widget.Clickable
	list  widget.List
	
	toggleSidebar widget.Clickable
	toggleMain    widget.Clickable
	toggleRecent  widget.Clickable
	toggleHideEmpty widget.Clickable
	toggleUnreadOnCollapse widget.Clickable
	toggleStatusBar widget.Clickable
	toggleLinkUnfurl  widget.Clickable
	toggleMediaUnfurl widget.Clickable
	toggleAutoScroll  widget.Clickable
}

type settingsRow struct {
	label   string
	target  *FontStyle
	mono    bool
	prevF   widget.Clickable
	nextF   widget.Clickable
	smaller widget.Clickable
	bigger  widget.Clickable
}

func newSettingsScreen(th *Theme, onChange, onClose func()) *SettingsScreen {
	s := &SettingsScreen{th: th, onChange: onChange, onClose: onClose}
	s.list.Axis = layout.Vertical
	s.rows = []*settingsRow{
		{label: "Global Font (Base)", target: &th.Fonts.Global},
		{label: "Channels sidebar", target: &th.Fonts.Channels},
		{label: "Channel header", target: &th.Fonts.Header},
		{label: "Messages", target: &th.Fonts.Messages},
		{label: "Thread replies", target: &th.Fonts.Threads},
		{label: "Composer", target: &th.Fonts.Composer},
		{label: "Code", target: &th.Fonts.Code, mono: true},
		{label: "Search (ctrl+k)", target: &th.Fonts.Search},
		{label: "User profile panel", target: &th.Fonts.UserInfo},
		{label: "Status Bar", target: &th.Fonts.StatusBar},
	}
	return s
}

// faces returns the typeface options offered for a given row. Code rows get
// the monospace-only subset so users don't accidentally pick a proportional
// face for code blocks.
func (s *SettingsScreen) faces(r *settingsRow) []string {
	if r.mono {
		return s.th.MonoFaces
	}
	return s.th.Faces
}

// cycleFace steps through the available typefaces by delta. Empty face
// (= "default") is treated as a virtual entry at index 0, so users can step
// back to the unset state.
func (s *SettingsScreen) cycleFace(r *settingsRow, delta int) {
	faces := s.faces(r)
	options := append([]string{""}, faces...)
	idx := 0
	for i, f := range options {
		if f == r.target.Face {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(options)) % len(options)
	r.target.Face = options[idx]
}

func (s *SettingsScreen) bumpSize(r *settingsRow, delta float32) {
	cur := r.target.Size
	if cur == 0 {
		cur = float32(s.th.Mat.TextSize)
	}
	cur += delta
	if cur < 8 {
		cur = 8
	}
	if cur > 32 {
		cur = 32
	}
	r.target.Size = cur
}

func (s *SettingsScreen) Layout(gtx layout.Context) layout.Dimensions {
	th := s.th
	dirty := false

	cycleTheme := func(current string) string {
		switch current {
		case "dark":
			return "light"
		case "light":
			return "linear"
		default:
			return "dark"
		}
	}

	if s.toggleSidebar.Clicked(gtx) {
		th.ThemeSidebar = cycleTheme(th.ThemeSidebar)
		th.ApplyThemePrefs(th.ThemeSidebar, th.ThemeMain)
		dirty = true
	}
	if s.toggleMain.Clicked(gtx) {
		th.ThemeMain = cycleTheme(th.ThemeMain)
		th.ApplyThemePrefs(th.ThemeSidebar, th.ThemeMain)
		dirty = true
	}
	if s.toggleRecent.Clicked(gtx) {
		th.ShowOnlyRecentChannels = !th.ShowOnlyRecentChannels
		dirty = true
	}
	if s.toggleHideEmpty.Clicked(gtx) {
		th.HideEmptyChannels = !th.HideEmptyChannels
		dirty = true
	}
	if s.toggleUnreadOnCollapse.Clicked(gtx) {
		th.ShowUnreadOnCollapse = !th.ShowUnreadOnCollapse
		dirty = true
	}
	if s.toggleStatusBar.Clicked(gtx) {
		th.ShowStatusBar = !th.ShowStatusBar
		dirty = true
	}
	if s.toggleLinkUnfurl.Clicked(gtx) {
		th.DisableLinkUnfurl = !th.DisableLinkUnfurl
		dirty = true
	}
	if s.toggleMediaUnfurl.Clicked(gtx) {
		th.DisableMediaUnfurl = !th.DisableMediaUnfurl
		dirty = true
	}
	if s.toggleAutoScroll.Clicked(gtx) {
		th.AutoScrollOnNewMessage = !th.AutoScrollOnNewMessage
		dirty = true
	}

	for _, r := range s.rows {
		if r.prevF.Clicked(gtx) {
			s.cycleFace(r, -1)
			dirty = true
		}
		if r.nextF.Clicked(gtx) {
			s.cycleFace(r, 1)
			dirty = true
		}
		if r.smaller.Clicked(gtx) {
			s.bumpSize(r, -1)
			dirty = true
		}
		if r.bigger.Clicked(gtx) {
			s.bumpSize(r, 1)
			dirty = true
		}
	}
	if s.close.Clicked(gtx) {
		if s.onClose != nil {
			s.onClose()
		}
	}
	if dirty && s.onChange != nil {
		s.onChange()
	}

	return paintedBg(gtx, th.Pal.Bg, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{
			Top:    unit.Dp(20),
			Bottom: unit.Dp(20),
			Left:   unit.Dp(28),
			Right:  unit.Dp(28),
		}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.layoutHeader(gtx, th)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.layoutThemeToggles(gtx, th)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(14)}.Layout),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return material.List(th.Mat, &s.list).Layout(gtx, len(s.rows), func(gtx layout.Context, i int) layout.Dimensions {
						return s.layoutRow(gtx, th, s.rows[i])
					})
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					hint := material.Caption(th.Mat, "Esc close | changes save automatically")
					hint.Color = th.Pal.TextMuted
					return hint.Layout(gtx)
				}),
			)
		})
	})
}

func (s *SettingsScreen) layoutThemeToggles(gtx layout.Context, th *Theme) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th.Mat, "Preferences")
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.TextStrong
			lbl.Font.Weight = font.SemiBold
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Left Panel (Sidebar)")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, th, &s.toggleSidebar, th.ThemeSidebar)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Right Panel (Main)")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.button(gtx, th, &s.toggleMain, th.ThemeMain)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Limit groups to 10 recent")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "off"
					if th.ShowOnlyRecentChannels {
						label = "on"
					}
					return s.button(gtx, th, &s.toggleRecent, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Hide empty channels")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "off"
					if th.HideEmptyChannels {
						label = "on"
					}
					return s.button(gtx, th, &s.toggleHideEmpty, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Show unread on collapse")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "off"
					if th.ShowUnreadOnCollapse {
						label = "on"
					}
					return s.button(gtx, th, &s.toggleUnreadOnCollapse, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Show Status Bar")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "off"
					if th.ShowStatusBar {
						label = "on"
					}
					return s.button(gtx, th, &s.toggleStatusBar, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Show Link Previews")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "on"
					if th.DisableLinkUnfurl {
						label = "off"
					}
					return s.button(gtx, th, &s.toggleLinkUnfurl, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Show Media Previews")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "on"
					if th.DisableMediaUnfurl {
						label = "off"
					}
					return s.button(gtx, th, &s.toggleMediaUnfurl, label)
				}),
			)
		}),
		layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, "Auto-scroll to new messages")
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextDim
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(160))
					return lbl.Layout(gtx)
				}),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					label := "off"
					if th.AutoScrollOnNewMessage {
						label = "on"
					}
					return s.button(gtx, th, &s.toggleAutoScroll, label)
				}),
			)
		}),
	)
}

func (s *SettingsScreen) layoutHeader(gtx layout.Context, th *Theme) layout.Dimensions {
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.H6(th.Mat, "Settings | Fonts")
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.TextStrong
			lbl.Font.Weight = font.Bold
			return lbl.Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions { return layout.Dimensions{Size: gtx.Constraints.Min} }),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return s.button(gtx, th, &s.close, "close")
		}),
	)
}

func (s *SettingsScreen) layoutRow(gtx layout.Context, th *Theme, r *settingsRow) layout.Dimensions {
	return withBorder(gtx, th.Pal.Border, borders{Bottom: true}, func(gtx layout.Context) layout.Dimensions {
		return layout.Inset{Top: unit.Dp(10), Bottom: unit.Dp(10)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body1(th.Mat, r.label)
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.TextStrong
					lbl.Font.Weight = font.SemiBold
					return lbl.Layout(gtx)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.layoutFaceControls(gtx, th, r)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.layoutSizeControls(gtx, th, r)
				}),
				layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.layoutPreview(gtx, th, r)
				}),
			)
		})
	})
}

func (s *SettingsScreen) layoutFaceControls(gtx layout.Context, th *Theme, r *settingsRow) layout.Dimensions {
	face := r.target.Face
	if face == "" {
		face = "default"
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th.Mat, "Face")
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.TextDim
			gtx.Constraints.Min.X = gtx.Dp(unit.Dp(48))
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, th, &r.prevF, "<") }),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th.Mat, face)
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.Text
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, th, &r.nextF, ">") }),
	)
}

func (s *SettingsScreen) layoutSizeControls(gtx layout.Context, th *Theme, r *settingsRow) layout.Dimensions {
	size := r.target.Size
	display := fmt.Sprintf("%.0f sp", size)
	if size == 0 {
		display = fmt.Sprintf("default (%d sp)", int(s.th.Mat.TextSize))
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body2(th.Mat, "Size")
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.TextDim
			gtx.Constraints.Min.X = gtx.Dp(unit.Dp(48))
			return lbl.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, th, &r.smaller, "-") }),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Body1(th.Mat, display)
			th.applyFont(&lbl, FontStyle{})
			lbl.Color = th.Pal.Text
			return lbl.Layout(gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions { return s.button(gtx, th, &r.bigger, "+") }),
	)
}

// layoutPreview shows a sample line in the row's current face/size so changes
// are visible before the user closes the screen.
func (s *SettingsScreen) layoutPreview(gtx layout.Context, th *Theme, r *settingsRow) layout.Dimensions {
	sample := "The quick brown fox jumps over the lazy dog"
	if r.mono {
		sample = "for i := 0; i < n; i++ { fmt.Println(i) }"
	}
	lbl := material.Body1(th.Mat, sample)
	lbl.Color = th.Pal.TextDim
	th.applyFont(&lbl, *r.target)
	return lbl.Layout(gtx)
}

func (s *SettingsScreen) button(gtx layout.Context, th *Theme, c *widget.Clickable, label string) layout.Dimensions {
	return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return withBorder(gtx, th.Pal.Border, borders{Top: true, Bottom: true, Left: true, Right: true}, func(gtx layout.Context) layout.Dimensions {
			return paintedBg(gtx, th.Pal.BgSidebar, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{
					Top:    unit.Dp(4),
					Bottom: unit.Dp(4),
					Left:   unit.Dp(10),
					Right:  unit.Dp(10),
				}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					lbl := material.Body2(th.Mat, label)
					th.applyFont(&lbl, FontStyle{})
					lbl.Color = th.Pal.Text
					return lbl.Layout(gtx)
				})
			})
		})
	})
}
