package slack

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Formatter struct {
	cache    *Cache
	emojiMap map[string]string
	tsFormat string
}

func NewFormatter(cache *Cache, tsFormat string) *Formatter {
	return &Formatter{
		cache:    cache,
		emojiMap: defaultEmojiMap(),
		tsFormat: tsFormat,
	}
}

// StyleFlag is a UI-toolkit-agnostic bitmask describing how a text span should
// render. The UI layer maps these to its own styling primitives.
type StyleFlag uint16

const (
	StyleBold StyleFlag = 1 << iota
	StyleItalic
	StyleStrike
	StyleCode      // inline code
	StyleCodeBlock // fenced code block
	StyleMention   // @user, @group, @here/@channel/@everyone
	StyleChannel   // #channel
	StyleLink      // URL link
	StyleStaging
	StyleProduction
	StyleResolved
	StyleFiring
)

// Span is a piece of styled text after Slack mrkdwn has been parsed. It is
// independent of any GUI toolkit; UI code decides colors and fonts.
type Span struct {
	Text  string
	Style StyleFlag
	Link  string // populated when Style&StyleLink != 0
}

// Format reduces Slack mrkdwn to a plain text string with all references
// resolved (mentions, channels, emoji shortcodes, URLs unwrapped). No styling
// information is preserved — this is for tooltips, log lines, and clipboards.
func (f *Formatter) Format(text string) string {
	text = collapseBlankLines(text)
	text = f.resolveUserMentions(text)
	text = f.resolveGroupMentions(text)
	text = f.resolveSpecialMentions(text)
	text = f.resolveChannelLinks(text)
	text = f.resolveURLsPlain(text)
	text = f.resolveEmoji(text)
	text = f.resolveHTMLEntities(text)
	return text
}

// FormatSpans returns the message body broken up into styled spans. The order
// of regex passes mirrors Format() but inline styling (bold/italic/strike/code)
// and link spans are recorded structurally rather than collapsed into the
// plain text.
func (f *Formatter) FormatSpans(text string) []Span {
	// Resolve references that produce plain text first.
	text = collapseBlankLines(text)
	text = f.resolveUserMentions(text)
	text = f.resolveGroupMentions(text)
	text = f.resolveSpecialMentions(text)
	text = f.resolveChannelLinks(text)
	text = f.resolveEmoji(text)
	text = f.resolveHTMLEntities(text)

	// Now slice into styled spans. We do a single tokenized walk: each iteration
	// finds the next styling marker and records the unstyled text before it,
	// then the styled run.
	return tokenize(text)
}

// FormatTimestamp converts a Slack timestamp to a human-readable time with age.
func (f *Formatter) FormatTimestamp(ts string) string {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return ts
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ts
	}
	t := time.Unix(sec, 0).Local()

	now := time.Now()
	var formatted string
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		formatted = t.Format(f.tsFormat)
	} else if t.Year() == now.Year() {
		formatted = t.Format("Jan 2 " + f.tsFormat)
	} else {
		formatted = t.Format("Jan 2, 2006 " + f.tsFormat)
	}

	return formatted + " (" + FormatAge(now.Sub(t)) + ")"
}

func (f *Formatter) FormatTimestampAge(ts string) string {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return ts
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ts
	}
	return FormatAge(time.Since(time.Unix(sec, 0)))
}

// FormatTimeOnly returns a formatted date/time string — used in message headers.
func (f *Formatter) FormatTimeOnly(ts string) string {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return ts
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ts
	}
	t := time.Unix(sec, 0).Local()

	now := time.Now()
	var formatted string
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		formatted = t.Format(f.tsFormat)
	} else if t.Year() == now.Year() {
		formatted = t.Format("Jan 2 " + f.tsFormat)
	} else {
		formatted = t.Format("Jan 2, 2006 " + f.tsFormat)
	}

	return formatted
}

func FormatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	case d < 30*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	case d < 365*24*time.Hour:
		months := int(d.Hours() / 24 / 30)
		if months <= 1 {
			return "1mo ago"
		}
		return fmt.Sprintf("%dmo ago", months)
	default:
		years := int(d.Hours() / 24 / 365)
		if years == 1 {
			return "1y ago"
		}
		return fmt.Sprintf("%dy ago", years)
	}
}

// EmojiEntry is a (name, glyph) pair for the reaction picker.
type EmojiEntry struct {
	Name  string
	Glyph string
}

// EmojiCatalog returns the formatter's known emoji shortcodes paired with
// their rendered glyph, sorted by name. The reaction picker uses this as the
// search corpus.
func (f *Formatter) EmojiCatalog() []EmojiEntry {
	out := make([]EmojiEntry, 0, len(f.emojiMap))
	for name, glyph := range f.emojiMap {
		out = append(out, EmojiEntry{Name: name, Glyph: glyph})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *Formatter) FormatEmoji(name string) string {
	base, _, _ := strings.Cut(name, "::")
	if emoji, ok := f.emojiMap[base]; ok {
		return emoji
	}
	return ":" + name + ":"
}

func (f *Formatter) FormatDate(ts string) string {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return ""
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(sec, 0).Local().Format("Monday, January 2, 2006")
}

func (f *Formatter) GetUser(userID string) *User {
	return f.cache.GetUser(userID)
}

var (
	reUserMention    = regexp.MustCompile(`<@(U[A-Z0-9]+)>`)
	reChannelLink    = regexp.MustCompile(`<#(C[A-Z0-9]+)\|([^>]+)>`)
	reURL            = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	reURLNoLabel     = regexp.MustCompile(`<(https?://[^>]+)>`)
	reEmojiShort     = regexp.MustCompile(`:([a-z0-9_+-]+):`)
	reSubteamLabel   = regexp.MustCompile(`<!subteam\^(S[A-Z0-9]+)\|@([^>]+)>`)
	reSubteamNoLabel = regexp.MustCompile(`<!subteam\^(S[A-Z0-9]+)>`)
	reSpecialMention = regexp.MustCompile(`<!(here|channel|everyone)(\|[^>]*)?>`)
	reCodeBlock      = regexp.MustCompile("(?s)```(.+?)```")
	reInlineCode     = regexp.MustCompile("`([^`]+)`")
	reBareURL        = regexp.MustCompile(`https?://[^\s<>]+`)
	reEnvironments   = regexp.MustCompile(`(?i)\b(staging|production)\b`)
	reAlertStatus    = regexp.MustCompile(`\b(RESOLVED|FIRING)\b`)
	reBlankLines     = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
)

// collapseBlankLines reduces any run of two or more newlines (with optional
// inline whitespace between them) down to a single newline.
func collapseBlankLines(text string) string {
	return reBlankLines.ReplaceAllString(text, "\n")
}

func (f *Formatter) resolveUserMentions(text string) string {
	return reUserMention.ReplaceAllStringFunc(text, func(match string) string {
		parts := reUserMention.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		userID := parts[1]
		if user := f.cache.GetUser(userID); user != nil {
			name := user.DisplayName
			if name == "" {
				name = user.Name
			}
			return fmt.Sprintf("@%s", name)
		}
		return fmt.Sprintf("@%s", userID)
	})
}

func (f *Formatter) resolveGroupMentions(text string) string {
	text = reSubteamLabel.ReplaceAllString(text, "@$2")
	text = reSubteamNoLabel.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSubteamNoLabel.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		groupID := parts[1]
		if group := f.cache.GetUserGroup(groupID); group != nil {
			return "@" + group.Handle
		}
		return "@" + groupID
	})
	return text
}

func (f *Formatter) resolveSpecialMentions(text string) string {
	return reSpecialMention.ReplaceAllString(text, "@$1")
}

func (f *Formatter) resolveChannelLinks(text string) string {
	return reChannelLink.ReplaceAllString(text, "#$2")
}

// resolveURLsPlain unwraps <url|label> -> label and <url> -> url, used by
// the plain-text Format() variant.
func (f *Formatter) resolveURLsPlain(text string) string {
	text = reURL.ReplaceAllString(text, "$2")
	text = reURLNoLabel.ReplaceAllString(text, "$1")
	return text
}

func (f *Formatter) resolveEmoji(text string) string {
	return reEmojiShort.ReplaceAllStringFunc(text, func(match string) string {
		parts := reEmojiShort.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return f.FormatEmoji(parts[1])
	})
}

func (f *Formatter) resolveHTMLEntities(text string) string {
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	return text
}

// ExtractURLs returns all URLs found in Slack mrkdwn text.
func ExtractURLs(text string) []string {
	var urls []string
	seen := make(map[string]bool)

	for _, m := range reURL.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 && !seen[m[1]] {
			urls = append(urls, m[1])
			seen[m[1]] = true
		}
	}
	for _, m := range reURLNoLabel.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 && !seen[m[1]] {
			urls = append(urls, m[1])
			seen[m[1]] = true
		}
	}
	return urls
}

// tokenize walks the (already de-referenced) text and emits styled spans.
// It handles, in priority order: code blocks, inline code, <url|label> and
// <url> link forms, bare http(s) URLs, *bold*, _italic_, ~strike~, and
// environment / alert-status keyword highlights.
func tokenize(text string) []Span {
	var out []Span
	emit := func(s string, style StyleFlag, link string) {
		if s == "" {
			return
		}
		out = append(out, Span{Text: s, Style: style, Link: link})
	}

	// Pull out code blocks first — their interior must not be re-parsed.
	pos := 0
	for pos < len(text) {
		m := reCodeBlock.FindStringIndex(text[pos:])
		if m == nil {
			break
		}
		start, end := pos+m[0], pos+m[1]
		appendInline(&out, text[pos:start])
		body := reCodeBlock.FindStringSubmatch(text[start:end])
		if len(body) >= 2 {
			emit(body[1], StyleCodeBlock, "")
		}
		pos = end
	}
	appendInline(&out, text[pos:])
	return out
}

// appendInline tokenizes a text run that has no fenced code blocks, then
// appends the resulting spans onto out.
func appendInline(out *[]Span, text string) {
	if text == "" {
		return
	}
	emit := func(s string, style StyleFlag, link string) {
		if s == "" {
			return
		}
		*out = append(*out, Span{Text: s, Style: style, Link: link})
	}

	pos := 0
	for pos < len(text) {
		// Try inline-code, link forms, bare URL, bold/italic/strike — pick
		// whichever starts soonest.
		type cand struct {
			start, end int
			emit       func()
		}
		var best *cand

		try := func(c cand) {
			if best == nil || c.start < best.start {
				cc := c
				best = &cc
			}
		}

		if m := reInlineCode.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			content := text[pos+m[2] : pos+m[3]]
			try(cand{s, e, func() { emit(content, StyleCode, "") }})
		}
		if m := reURL.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			url := text[pos+m[2] : pos+m[3]]
			label := text[pos+m[4] : pos+m[5]]
			try(cand{s, e, func() { emit(label, StyleLink, url) }})
		}
		if m := reURLNoLabel.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			url := text[pos+m[2] : pos+m[3]]
			try(cand{s, e, func() { emit(url, StyleLink, url) }})
		}
		if m := reBareURL.FindStringIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			url := text[s:e]
			try(cand{s, e, func() { emit(url, StyleLink, url) }})
		}
		if loc := findInline(text, pos, '*'); loc != nil {
			try(cand{loc[0], loc[1], func() { emit(text[loc[0]+1:loc[1]-1], StyleBold, "") }})
		}
		if loc := findInline(text, pos, '_'); loc != nil {
			try(cand{loc[0], loc[1], func() { emit(text[loc[0]+1:loc[1]-1], StyleItalic, "") }})
		}
		if loc := findInline(text, pos, '~'); loc != nil {
			try(cand{loc[0], loc[1], func() { emit(text[loc[0]+1:loc[1]-1], StyleStrike, "") }})
		}
		// keyword highlights
		if m := reEnvironments.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			word := text[s:e]
			style := StyleStaging
			if strings.EqualFold(word, "production") {
				style = StyleProduction
			}
			try(cand{s, e, func() { emit(word, style, "") }})
		}
		if m := reAlertStatus.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			word := text[s:e]
			style := StyleResolved
			if word == "FIRING" {
				style = StyleFiring
			}
			try(cand{s, e, func() { emit(word, style, "") }})
		}

		if best == nil {
			emit(text[pos:], 0, "")
			return
		}
		emit(text[pos:best.start], 0, "")
		best.emit()
		pos = best.end
	}
}

// findInline finds the smallest delim...delim run starting at or after pos
// where the delim is preceded by a word boundary and the body is non-empty
// and doesn't start/end with whitespace. Returns [start, end) (end exclusive).
func findInline(text string, pos int, delim byte) []int {
	for i := pos; i < len(text); i++ {
		if text[i] != delim {
			continue
		}
		if i > 0 {
			prev := text[i-1]
			if isAlphanumeric(prev) || prev == delim {
				continue
			}
		}
		// scan for closing delim
		for j := i + 1; j < len(text); j++ {
			if text[j] != delim {
				continue
			}
			body := text[i+1 : j]
			if body == "" {
				break
			}
			// reject if body starts/ends with whitespace
			if body[0] == ' ' || body[0] == '\t' || body[0] == '\n' {
				break
			}
			if body[len(body)-1] == ' ' || body[len(body)-1] == '\t' || body[len(body)-1] == '\n' {
				continue
			}
			// reject if next char is alphanumeric (not a real close)
			if j+1 < len(text) && isAlphanumeric(text[j+1]) {
				continue
			}
			return []int{i, j + 1}
		}
		return nil
	}
	return nil
}

func isAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func defaultEmojiMap() map[string]string {
	return map[string]string{
		"thumbsup":              "\U0001F44D",
		"+1":                    "\U0001F44D",
		"thumbsdown":            "\U0001F44E",
		"-1":                    "\U0001F44E",
		"heart":                 "❤",
		"smile":                 "\U0001F604",
		"laughing":              "\U0001F606",
		"grinning":              "\U0001F600",
		"joy":                   "\U0001F602",
		"rofl":                  "\U0001F923",
		"wink":                  "\U0001F609",
		"blush":                 "\U0001F60A",
		"thinking_face":         "\U0001F914",
		"eyes":                  "\U0001F440",
		"fire":                  "\U0001F525",
		"100":                   "\U0001F4AF",
		"tada":                  "\U0001F389",
		"rocket":                "\U0001F680",
		"wave":                  "\U0001F44B",
		"pray":                  "\U0001F64F",
		"clap":                  "\U0001F44F",
		"raised_hands":          "\U0001F64C",
		"ok_hand":               "\U0001F44C",
		"point_up":              "☝",
		"point_down":            "\U0001F447",
		"point_left":            "\U0001F448",
		"point_right":           "\U0001F449",
		"muscle":                "\U0001F4AA",
		"white_check_mark":      "✅",
		"heavy_check_mark":      "✔",
		"x":                     "❌",
		"warning":               "⚠",
		"question":              "❓",
		"exclamation":           "❗",
		"bulb":                  "\U0001F4A1",
		"memo":                  "\U0001F4DD",
		"wrench":                "\U0001F527",
		"gear":                  "⚙",
		"bug":                   "\U0001F41B",
		"star":                  "⭐",
		"sparkles":              "✨",
		"zap":                   "⚡",
		"sunny":                 "☀",
		"cloud":                 "☁",
		"umbrella":              "☂",
		"coffee":                "☕",
		"beer":                  "\U0001F37A",
		"pizza":                 "\U0001F355",
		"taco":                  "\U0001F32E",
		"green_heart":           "\U0001F49A",
		"blue_heart":            "\U0001F499",
		"purple_heart":          "\U0001F49C",
		"broken_heart":          "\U0001F494",
		"skull":                 "\U0001F480",
		"ghost":                 "\U0001F47B",
		"robot_face":            "\U0001F916",
		"see_no_evil":           "\U0001F648",
		"hear_no_evil":          "\U0001F649",
		"speak_no_evil":         "\U0001F64A",
		"sob":                   "\U0001F62D",
		"cry":                   "\U0001F622",
		"angry":                 "\U0001F620",
		"rage":                  "\U0001F621",
		"sweat_smile":           "\U0001F605",
		"sweat":                 "\U0001F613",
		"grimacing":             "\U0001F62C",
		"relieved":              "\U0001F60C",
		"unamused":              "\U0001F612",
		"disappointed":          "\U0001F61E",
		"confused":              "\U0001F615",
		"sleeping":              "\U0001F634",
		"sunglasses":            "\U0001F60E",
		"nerd_face":             "\U0001F913",
		"party_popper":          "\U0001F389",
		"confetti_ball":         "\U0001F38A",
		"balloon":               "\U0001F388",
		"gift":                  "\U0001F381",
		"trophy":                "\U0001F3C6",
		"medal":                 "\U0001F3C5",
		"crown":                 "\U0001F451",
		"gem":                   "\U0001F48E",
		"lock":                  "\U0001F512",
		"key":                   "\U0001F511",
		"link":                  "\U0001F517",
		"paperclip":             "\U0001F4CE",
		"scissors":              "✂",
		"hammer":                "\U0001F528",
		"hammer_and_wrench":     "\U0001F6E0",
		"hourglass":             "⌛",
		"stopwatch":             "⏱",
		"alarm_clock":           "⏰",
		"calendar":              "\U0001F4C5",
		"pushpin":               "\U0001F4CC",
		"round_pushpin":         "\U0001F4CD",
		"mag":                   "\U0001F50D",
		"bell":                  "\U0001F514",
		"no_bell":               "\U0001F515",
		"speech_balloon":        "\U0001F4AC",
		"thought_balloon":       "\U0001F4AD",
		"arrow_up":              "⬆",
		"arrow_down":            "⬇",
		"arrow_left":            "⬅",
		"arrow_right":           "➡",
		"heavy_plus_sign":       "➕",
		"heavy_minus_sign":      "➖",
		"wavy_dash":             "〰",
		"slightly_smiling_face": "\U0001F642",
		"upside_down_face":      "\U0001F643",
		"stuck_out_tongue":      "\U0001F61B",
		"octagonal_sign":        "\U0001F6D1",
		"no_entry":              "⛔",
		"no_entry_sign":         "\U0001F6AB",
		"red_circle":            "\U0001F534",
		"large_blue_circle":     "\U0001F535",
		"green_circle":          "\U0001F7E2",
		"yellow_circle":         "\U0001F7E1",
		"orange_circle":         "\U0001F7E0",
		"black_circle":          "⚫",
		"white_circle":          "⚪",
		"large_green_square":    "\U0001F7E9",
		"large_yellow_square":   "\U0001F7E8",
		"large_red_square":      "\U0001F7E5",
		"large_blue_square":     "\U0001F7E6",
		"shrug":                 "\U0001F937",
		"face_palm":             "\U0001F926",
		"facepalm":              "\U0001F926",
		"thinking":              "\U0001F914",
		"woman-shrugging":       "\U0001F937‍♀️",
		"man-shrugging":         "\U0001F937‍♂️",
		"smiley":                "\U0001F603",
		"neutral_face":          "\U0001F610",
		"expressionless":        "\U0001F611",
		"flushed":               "\U0001F633",
		"open_mouth":            "\U0001F62E",
		"hushed":                "\U0001F62F",
		"astonished":            "\U0001F632",
		"scream":                "\U0001F631",
		"persevere":             "\U0001F623",
		"pleading_face":         "\U0001F97A",
		"smiling_face_with_tear": "\U0001F972",
		"partying_face":         "\U0001F973",
		"smiling_imp":           "\U0001F608",
		"imp":                   "\U0001F47F",
		"alien":                 "\U0001F47D",
		"poop":                  "\U0001F4A9",
		"hankey":                "\U0001F4A9",
		"hand":                  "✋",
		"raised_hand":           "✋",
		"raised_back_of_hand":   "\U0001F91A",
		"vulcan_salute":         "\U0001F596",
		"crossed_fingers":       "\U0001F91E",
		"v":                     "✌",
		"metal":                 "\U0001F918",
		"call_me_hand":          "\U0001F919",
		"writing_hand":          "✍",
		"selfie":                "\U0001F933",
		"open_hands":            "\U0001F450",
		"handshake":             "\U0001F91D",
		"folded_hands":          "\U0001F64F",
		"dancer":                "\U0001F483",
		"man_dancing":           "\U0001F57A",
		"running":               "\U0001F3C3",
		"walking":               "\U0001F6B6",
		"bow":                   "\U0001F647",
		"information_source":    "ℹ",
		"recycle":               "♻",
		"radioactive_sign":      "☢",
		"biohazard":             "☣",
		"trident":               "\U0001F531",
		"name_badge":            "\U0001F4DB",
		"beginner":              "\U0001F530",
		"o":                     "⭕",
		"o2":                    "\U0001F17E️",
		"sos":                   "\U0001F198",
		"checkered_flag":        "\U0001F3C1",
		"triangular_flag_on_post": "\U0001F6A9",
		"crossed_flags":         "\U0001F38C",
		"black_flag":            "\U0001F3F4",
		"white_flag":            "\U0001F3F3",
		"siren":                 "\U0001F6A8",
		"rotating_light":        "\U0001F6A8",
		"police_car":            "\U0001F693",
		"ambulance":             "\U0001F691",
		"fire_engine":           "\U0001F692",
		"construction":          "\U0001F6A7",
		"no_good":               "\U0001F645",
		"ok_woman":              "\U0001F646",
		"raising_hand":          "\U0001F64B",
		"bow_and_arrow":         "\U0001F3F9",
		"shield":                "\U0001F6E1",
		"chart_with_upwards_trend": "\U0001F4C8",
		"chart_with_downwards_trend": "\U0001F4C9",
		"bar_chart":             "\U0001F4CA",
		"clipboard":             "\U0001F4CB",
		"date":                  "\U0001F4C5",
		"chart":                 "\U0001F4B9",
		"book":                  "\U0001F4D6",
		"books":                 "\U0001F4DA",
		"computer":              "\U0001F4BB",
		"desktop_computer":      "\U0001F5A5",
		"keyboard":              "⌨",
		"mouse":                 "\U0001F5B1",
		"floppy_disk":           "\U0001F4BE",
		"cd":                    "\U0001F4BF",
		"dvd":                   "\U0001F4C0",
		"package":               "\U0001F4E6",
		"mailbox":               "\U0001F4EB",
		"email":                 "\U0001F4E7",
		"envelope":              "✉",
		"phone":                 "\U0001F4DE",
		"iphone":                "\U0001F4F1",
		"telephone_receiver":    "\U0001F4DE",
		"speaker":               "\U0001F50A",
		"loudspeaker":           "\U0001F4E2",
		"mega":                  "\U0001F4E3",
		"mute":                  "\U0001F507",
		"sound":                 "\U0001F509",
		"loud_sound":            "\U0001F50A",
		"musical_note":          "\U0001F3B5",
		"notes":                 "\U0001F3B6",
		"saxophone":             "\U0001F3B7",
		"guitar":                "\U0001F3B8",
		"trumpet":               "\U0001F3BA",
		"violin":                "\U0001F3BB",
		"drum_with_drumsticks":  "\U0001F941",
		"microphone":            "\U0001F3A4",
		"headphones":            "\U0001F3A7",
		"radio":                 "\U0001F4FB",
		"tv":                    "\U0001F4FA",
		"camera":                "\U0001F4F7",
		"video_camera":          "\U0001F4F9",
		"clipboard2":            "\U0001F4CB",
		"hammer_and_pick":       "⚒",
		"crossed_swords":        "⚔",
		"gun":                   "\U0001F52B",
		"bomb":                  "\U0001F4A3",
		"smoking":               "\U0001F6AC",
		"coffin":                "⚰",
		"funeral_urn":           "⚱",
		"amphora":               "\U0001F3FA",
		"crystal_ball":          "\U0001F52E",
		"prayer_beads":          "\U0001F4FF",
		"barber":                "\U0001F488",
		"alembic":               "⚗",
		"telescope":             "\U0001F52D",
		"microscope":            "\U0001F52C",
		"hole":                  "\U0001F573",
		"pill":                  "\U0001F48A",
		"syringe":               "\U0001F489",
		"thermometer":           "\U0001F321",
		"label":                 "\U0001F3F7",
		"newspaper":             "\U0001F4F0",
		"page_facing_up":        "\U0001F4C4",
		"page_with_curl":        "\U0001F4C3",
		"bookmark":              "\U0001F516",
		"bookmark_tabs":         "\U0001F4D1",
		"ledger":                "\U0001F4D2",
		"notebook":              "\U0001F4D3",
		"notebook_with_decorative_cover": "\U0001F4D4",
		"closed_book":           "\U0001F4D5",
		"green_book":            "\U0001F4D7",
		"blue_book":             "\U0001F4D8",
		"orange_book":           "\U0001F4D9",
		"open_book":             "\U0001F4D6",
		"file_folder":           "\U0001F4C1",
		"open_file_folder":      "\U0001F4C2",
		"card_index_dividers":   "\U0001F5C2",
		"card_index":            "\U0001F4C7",
		"card_file_box":         "\U0001F5C3",
		"file_cabinet":          "\U0001F5C4",
		"wastebasket":           "\U0001F5D1",
		"spiral_notepad":        "\U0001F5D2",
		"spiral_calendar":       "\U0001F5D3",
		"chart_with_upwards_trend2": "\U0001F4C8",
		"sleeping_accommodation": "\U0001F6CC",
		"100pct":                "\U0001F4AF",
	}
}
