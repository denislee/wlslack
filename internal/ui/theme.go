package ui

import (
	"image/color"
	"log/slog"
	"os"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
	"gioui.org/widget/material"
)

// Palette is a small set of colors used throughout the app. Kept minimal on
// purpose; matches a Slack-ish dark theme.
type Palette struct {
	Bg          color.NRGBA
	BgSidebar   color.NRGBA
	BgHeader    color.NRGBA
	BgComposer  color.NRGBA
	BgCode      color.NRGBA
	Text        color.NRGBA
	TextDim     color.NRGBA
	Accent      color.NRGBA
	AccentText  color.NRGBA
	Mention     color.NRGBA
	Link        color.NRGBA
	Unread      color.NRGBA
	Border      color.NRGBA
	Staging     color.NRGBA
	Production  color.NRGBA
	Resolved    color.NRGBA
	Firing      color.NRGBA
	Selection   color.NRGBA
}

func defaultPalette() Palette {
	return Palette{
		Bg:         rgb(0x1a1d21),
		BgSidebar:  rgb(0x19171d),
		BgHeader:   rgb(0x222529),
		BgComposer: rgb(0x222529),
		BgCode:     rgb(0x2c2f33),
		Text:       rgb(0xd1d2d3),
		TextDim:    rgb(0x8b8d8f),
		Accent:     rgb(0x1164a3),
		AccentText: rgb(0xffffff),
		Mention:    rgb(0xfbb33f),
		Link:       rgb(0x4ea0e1),
		Unread:     rgb(0xe01e5a),
		Border:     rgb(0x2c2d30),
		Staging:    rgb(0x6cb1ff),
		Production: rgb(0xff9f3a),
		Resolved:   rgb(0x2eb67d),
		Firing:     rgb(0xe01e5a),
		Selection:  rgb(0x1164a3),
	}
}

// Theme bundles the gioui Material theme with our palette.
type Theme struct {
	Mat     *material.Theme
	Pal     Palette
	BoldF   font.Font
	ItalicF font.Font
	MonoF   font.Font
}

func newTheme() *Theme {
	mat := material.NewTheme()
	collection := gofont.Collection()
	collection = append(collection, loadEmojiFaces()...)
	mat.Shaper = text.NewShaper(text.WithCollection(collection))
	mat.Palette.Bg = rgb(0x1a1d21)
	mat.Palette.Fg = rgb(0xd1d2d3)
	mat.Palette.ContrastBg = rgb(0x1164a3)
	mat.Palette.ContrastFg = rgb(0xffffff)
	return &Theme{
		Mat:     mat,
		Pal:     defaultPalette(),
		BoldF:   font.Font{Weight: font.Bold},
		ItalicF: font.Font{Style: font.Italic},
		MonoF:   font.Font{Typeface: "Go Mono"},
	}
}

// loadEmojiFaces tries common system locations for an emoji font and returns
// any successfully parsed faces. Used as fallback so the shaper has glyphs
// for emoji codepoints that the bundled Go fonts lack.
func loadEmojiFaces() []font.FontFace {
	candidates := []string{
		"/usr/share/fonts/noto/NotoColorEmoji.ttf",
		"/usr/share/fonts/truetype/noto/NotoColorEmoji.ttf",
		"/usr/share/fonts/twemoji/twemoji.ttf",
		"/usr/share/fonts/TTF/NotoColorEmoji.ttf",
		"/System/Library/Fonts/Apple Color Emoji.ttc",
	}
	var out []font.FontFace
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		faces, err := opentype.ParseCollection(data)
		if err != nil {
			slog.Warn("emoji font parse failed", "path", p, "error", err)
			continue
		}
		out = append(out, faces...)
	}
	return out
}

func rgb(hex uint32) color.NRGBA {
	return color.NRGBA{
		R: uint8(hex >> 16),
		G: uint8(hex >> 8),
		B: uint8(hex),
		A: 0xff,
	}
}
