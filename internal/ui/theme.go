package ui

import (
	"image/color"
	"log/slog"
	"os"
	"sort"
	"strings"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
)

// Palette is a small set of colors used throughout the app. Kept minimal on
// purpose; tuned for a calm, neutral dark surface where contrast comes from
// typography and 1px borders rather than heavy fills.
type Palette struct {
	Bg          color.NRGBA
	BgSidebar   color.NRGBA
	BgHeader    color.NRGBA
	BgComposer  color.NRGBA
	BgCode      color.NRGBA
	BgRowAlt    color.NRGBA // subtle hover/active row tint, no animation
	Text        color.NRGBA
	TextStrong  color.NRGBA
	TextDim     color.NRGBA
	TextMuted   color.NRGBA
	Accent      color.NRGBA
	AccentText  color.NRGBA
	Mention     color.NRGBA
	Link        color.NRGBA
	Unread      color.NRGBA
	Border      color.NRGBA
	BorderStrong color.NRGBA
	Staging     color.NRGBA
	Production  color.NRGBA
	Resolved    color.NRGBA
	Firing      color.NRGBA
	Selection   color.NRGBA
}

func darkPalette() Palette {
	return Palette{
		Bg:           rgb(0x0f1115),
		BgSidebar:    rgb(0x0b0c10),
		BgHeader:     rgb(0x0f1115),
		BgComposer:   rgb(0x0f1115),
		BgCode:       rgb(0x191c22),
		BgRowAlt:     rgb(0x16191f),
		Text:         rgb(0xd7dae0),
		TextStrong:   rgb(0xeef0f3),
		TextDim:      rgb(0x8a93a0),
		TextMuted:    rgb(0x5e6571),
		Accent:       rgb(0x5294ff),
		AccentText:   rgb(0xffffff),
		Mention:      rgb(0xf5b342),
		Link:         rgb(0x7aa9ff),
		Unread:       rgb(0xef476f),
		Border:       rgb(0x1c2027),
		BorderStrong: rgb(0x262b33),
		Staging:      rgb(0x6cb1ff),
		Production:   rgb(0xff9f3a),
		Resolved:     rgb(0x4cc38a),
		Firing:       rgb(0xef476f),
		Selection:    rgb(0x5294ff),
	}
}

func lightPalette() Palette {
	return Palette{
		Bg:           rgb(0xffffff),
		BgSidebar:    rgb(0xf4f5f7),
		BgHeader:     rgb(0xffffff),
		BgComposer:   rgb(0xffffff),
		BgCode:       rgb(0xf4f5f7),
		BgRowAlt:     rgb(0xeef0f3),
		Text:         rgb(0x191c22),
		TextStrong:   rgb(0x0b0c10),
		TextDim:      rgb(0x5e6571),
		TextMuted:    rgb(0x8a93a0),
		Accent:       rgb(0x0052cc),
		AccentText:   rgb(0xffffff),
		Mention:      rgb(0x0052cc),
		Link:         rgb(0x0052cc),
		Unread:       rgb(0xef476f),
		Border:       rgb(0xeef0f3),
		BorderStrong: rgb(0xd7dae0),
		Staging:      rgb(0x6cb1ff),
		Production:   rgb(0xff9f3a),
		Resolved:     rgb(0x4cc38a),
		Firing:       rgb(0xef476f),
		Selection:    rgb(0xa1c4ff),
	}
}

// FontStyle is the per-section typeface + size used when rendering labels.
// Face "" or Size 0 means "fall back to the theme default".
type FontStyle struct {
	Face string
	Size float32
}

// SectionFonts groups the user-configurable styles for each part of the UI.
// Adding a section here means: surface it in the Settings screen, add a
// FontPref field in config.FontPrefs, and apply it in the relevant component.
type SectionFonts struct {
	Channels FontStyle
	Header   FontStyle
	Messages FontStyle
	Composer FontStyle
	Code     FontStyle
	Search   FontStyle
	UserInfo FontStyle
}

// Theme bundles the gioui Material theme with our palette.
type Theme struct {
	Mat        *material.Theme
	Pal        Palette
	SidebarPal Palette
	ThemeSidebar string
	ThemeMain    string
	BoldF      font.Font
	ItalicF    font.Font
	MonoF      font.Font

	// Faces lists the unique typeface names available to the shaper, sorted
	// alphabetically. The Settings screen cycles through this list.
	Faces []string
	// MonoFaces is the subset of Faces that contain mono in the typeface name
	// (used for the Code section). Falls back to Faces when none match.
	MonoFaces []string

	// Fonts holds the live per-section overrides. Settings mutates this in
	// place; the next Layout pass picks up the new values.
	Fonts SectionFonts
}

func newTheme() *Theme {
	mat := material.NewTheme()
	// Density: 13sp body reads well in a chat app and tightens vertical rhythm.
	mat.TextSize = unit.Sp(13)

	collection := gofont.Collection()
	collection = append(collection, loadSystemFaces()...)
	collection = append(collection, loadEmojiFaces()...)
	mat.Shaper = text.NewShaper(text.WithCollection(collection))

	pal := darkPalette()
	mat.Palette.Bg = pal.Bg
	mat.Palette.Fg = pal.Text
	mat.Palette.ContrastBg = pal.Accent
	mat.Palette.ContrastFg = pal.AccentText

	monoFace := "Go Mono"
	if hasFace(collection, "JetBrains Mono") {
		monoFace = "JetBrains Mono"
	} else if hasFace(collection, "IBM Plex Mono") {
		monoFace = "IBM Plex Mono"
	}

	faces, monoFaces := uniqueFaces(collection)

	uiDefault := FontStyle{Size: 13}
	monoDefault := FontStyle{Face: monoFace, Size: 13}

	return &Theme{
		Mat:        mat,
		Pal:        pal,
		SidebarPal: pal,
		BoldF:      font.Font{Weight: font.Bold},
		ItalicF:   font.Font{Style: font.Italic},
		MonoF:     font.Font{Typeface: font.Typeface(monoFace)},
		Faces:     faces,
		MonoFaces: monoFaces,
		Fonts: SectionFonts{
			Channels: uiDefault,
			Header:   uiDefault,
			Messages: uiDefault,
			Composer: uiDefault,
			Code:     monoDefault,
		},
	}
}

// applyFont mutates a material.LabelStyle to honor a section's typeface and
// size, leaving zero fields untouched so theme defaults survive.
func (t *Theme) applyFont(lbl *material.LabelStyle, fs FontStyle) {
	if fs.Face != "" {
		lbl.Font.Typeface = font.Typeface(fs.Face)
	}
	if fs.Size > 0 {
		lbl.TextSize = unit.Sp(fs.Size)
	}
}

func (t *Theme) applyEditorFont(ed *material.EditorStyle, fs FontStyle) {
	if fs.Face != "" {
		ed.Font.Typeface = font.Typeface(fs.Face)
	}
	if fs.Size > 0 {
		ed.TextSize = unit.Sp(fs.Size)
	}
}

// FontFor returns the font.Font + size to use for a non-material widget (e.g.
// richtext spans) for the given section.
func (t *Theme) FontFor(fs FontStyle) (font.Font, unit.Sp) {
	f := font.Font{}
	if fs.Face != "" {
		f.Typeface = font.Typeface(fs.Face)
	}
	size := t.Mat.TextSize
	if fs.Size > 0 {
		size = unit.Sp(fs.Size)
	}
	return f, size
}

// ApplyFontPrefs overlays user prefs onto the theme defaults. Called at boot
// after newTheme, and again whenever Settings persists a change.
func (t *Theme) ApplyFontPrefs(p sectionPrefs) {
	apply := func(target *FontStyle, pref FontStyle) {
		if pref.Face != "" {
			target.Face = pref.Face
		}
		if pref.Size > 0 {
			target.Size = pref.Size
		}
	}
	apply(&t.Fonts.Channels, p.Channels)
	apply(&t.Fonts.Header, p.Header)
	apply(&t.Fonts.Messages, p.Messages)
	apply(&t.Fonts.Composer, p.Composer)
	apply(&t.Fonts.Code, p.Code)
	apply(&t.Fonts.Search, p.Search)
	apply(&t.Fonts.UserInfo, p.UserInfo)
}

func (t *Theme) ApplyThemePrefs(sidebarTheme, mainTheme string) {
	if sidebarTheme == "light" {
		t.SidebarPal = lightPalette()
	} else {
		t.SidebarPal = darkPalette()
	}

	if mainTheme == "light" {
		t.Pal = lightPalette()
	} else {
		t.Pal = darkPalette()
	}

	t.Mat.Palette.Bg = t.Pal.Bg
	t.Mat.Palette.Fg = t.Pal.Text
	t.Mat.Palette.ContrastBg = t.Pal.Accent
	t.Mat.Palette.ContrastFg = t.Pal.AccentText
}

// sectionPrefs mirrors config.FontPrefs without importing it here, so the
// ui package stays self-contained for tests.
type sectionPrefs struct {
	Channels FontStyle
	Header   FontStyle
	Messages FontStyle
	Composer FontStyle
	Code     FontStyle
	Search   FontStyle
	UserInfo FontStyle
}

// uniqueFaces collapses the loaded font collection into a sorted, deduplicated
// list of typeface names. monoFaces is the subset whose name contains "mono"
// (case-insensitive) — handy for the Code section dropdown.
func uniqueFaces(coll []font.FontFace) (faces []string, mono []string) {
	seen := map[string]bool{}
	for _, f := range coll {
		name := string(f.Font.Typeface)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		faces = append(faces, name)
	}
	sort.Strings(faces)
	for _, name := range faces {
		if strings.Contains(strings.ToLower(name), "mono") {
			mono = append(mono, name)
		}
	}
	if len(mono) == 0 {
		mono = faces
	}
	return faces, mono
}

// loadSystemFaces tries common system locations for sharper UI fonts and
// returns any successfully parsed faces. Inter / IBM Plex / JetBrains Mono
// are preferred over the bundled Go fonts when present because they're
// designed for UI body sizes.
func loadSystemFaces() []font.FontFace {
	candidates := []string{
		// Inter
		"/usr/share/fonts/inter/Inter-Regular.otf",
		"/usr/share/fonts/inter/Inter-Bold.otf",
		"/usr/share/fonts/inter/Inter-Italic.otf",
		"/usr/share/fonts/TTF/Inter-Regular.ttf",
		"/usr/share/fonts/TTF/Inter-Bold.ttf",
		"/usr/share/fonts/TTF/Inter-Italic.ttf",
		"/usr/share/fonts/truetype/inter/Inter-Regular.ttf",
		"/usr/share/fonts/truetype/inter/Inter-Bold.ttf",
		"/usr/share/fonts/truetype/inter/Inter-Italic.ttf",
		// IBM Plex Sans
		"/usr/share/fonts/ibm-plex/IBMPlexSans-Regular.otf",
		"/usr/share/fonts/ibm-plex/IBMPlexSans-Bold.otf",
		"/usr/share/fonts/ibm-plex/IBMPlexSans-Italic.otf",
		"/usr/share/fonts/TTF/IBMPlexSans-Regular.ttf",
		// JetBrains Mono
		"/usr/share/fonts/jetbrains-mono/JetBrainsMono-Regular.ttf",
		"/usr/share/fonts/TTF/JetBrainsMono-Regular.ttf",
		"/usr/share/fonts/truetype/jetbrains-mono/JetBrainsMono-Regular.ttf",
		// IBM Plex Mono fallback
		"/usr/share/fonts/ibm-plex/IBMPlexMono-Regular.otf",
	}
	var out []font.FontFace
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		faces, err := opentype.ParseCollection(data)
		if err != nil {
			slog.Warn("ui font parse failed", "path", p, "error", err)
			continue
		}
		out = append(out, faces...)
	}
	return out
}

func hasFace(coll []font.FontFace, name string) bool {
	for _, f := range coll {
		if string(f.Font.Typeface) == name {
			return true
		}
	}
	return false
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
