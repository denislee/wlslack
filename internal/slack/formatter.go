package slack

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kyokomi/emoji/v2"
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

func (f *Formatter) SetCustomEmojis(emojis map[string]string) {
	for name, val := range emojis {
		// handle aliases
		for strings.HasPrefix(val, "alias:") {
			alias := strings.TrimPrefix(val, "alias:")
			if real, ok := emojis[alias]; ok {
				val = real
			} else if real, ok := f.emojiMap[alias]; ok {
				val = real
				break
			} else {
				break
			}
		}
		f.emojiMap[name] = val
	}
}

func (f *Formatter) IsCustomEmoji(name string) bool {
	val, ok := f.emojiMap[name]
	if !ok {
		return false
	}
	return strings.HasPrefix(val, "http")
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
	text = f.resolveEmoji(text)
	text = f.resolveHTMLEntities(text)

	// Now slice into styled spans. We do a single tokenized walk: each iteration
	// finds the next styling marker and records the unstyled text before it,
	// then the styled run.
	return f.tokenize(text)
}

func ParseTimestamp(ts string) (time.Time, bool) {
	parts := strings.SplitN(ts, ".", 2)
	if len(parts) == 0 {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).Local(), true
}

// FormatTimestamp converts a Slack timestamp to a human-readable time with age.
func (f *Formatter) FormatTimestamp(ts string) string {
	t, ok := ParseTimestamp(ts)
	if !ok {
		return ts
	}

	now := time.Now()
	var formatted string
	if t.Year() == now.Year() {
		formatted = t.Format("Jan 2 " + f.tsFormat)
	} else {
		formatted = t.Format("Jan 2, 2006 " + f.tsFormat)
	}

	return formatted + " (" + FormatAge(now.Sub(t)) + ")"
}

func (f *Formatter) FormatTimestampAge(ts string) string {
	t, ok := ParseTimestamp(ts)
	if !ok {
		return ts
	}
	return FormatAge(time.Since(t))
}

// FormatTimeOnly returns a formatted date/time string — used in message headers.
func (f *Formatter) FormatTimeOnly(ts string) string {
	t, ok := ParseTimestamp(ts)
	if !ok {
		return ts
	}

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

func (f *Formatter) GetAllUsers() []User {
	return f.cache.GetAllUsers()
}

func (f *Formatter) GetAllUserGroups() []UserGroup {
	return f.cache.GetAllUserGroups()
}

var (
	reUserMention    = regexp.MustCompile(`<@([A-Z0-9]+)(\|[^>]+)?>`)
	reChannelLink    = regexp.MustCompile(`<#(C[A-Z0-9]+)\|([^>]+)>`)
	reURL            = regexp.MustCompile(`<(https?://[^|>]+)\|([^>]+)>`)
	reURLNoLabel     = regexp.MustCompile(`<(https?://[^>]+)>`)
	reEmojiShort     = regexp.MustCompile(`:([a-z0-9_+-]+):`)
	reSubteamLabel   = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)\|@?([^>]+)>`)
	reSubteamNoLabel = regexp.MustCompile(`<!subteam\^([A-Z0-9]+)>`)
	reTeamMention    = regexp.MustCompile(`<!team\^([A-Z0-9]+)>`)
	reSpecialMention = regexp.MustCompile(`<!(here|channel|everyone)(\|[^>]*)?>`)
	reCodeBlock      = regexp.MustCompile("(?s)```(.+?)```")
	reInlineCode     = regexp.MustCompile("`([^`]+)`")
	reBareURL        = regexp.MustCompile(`https?://[^\s<>]+`)
	reEnvironments   = regexp.MustCompile(`(?i)\b(staging|production)\b`)
	reAlertStatus    = regexp.MustCompile(`\b(RESOLVED|FIRING)\b`)
	reBlankLines     = regexp.MustCompile(`([ \t]*\r?\n[ \t]*){2,}`)
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
		id := parts[1]
		label := ""
		if len(parts) > 2 && parts[2] != "" {
			label = strings.TrimPrefix(parts[2], "|")
		}

		if strings.HasPrefix(id, "S") {
			if f.cache != nil {
				if group := f.cache.GetUserGroup(id); group != nil {
					return "@" + group.Handle
				}
			}
			if label != "" {
				return "@" + label
			}
			return "@" + id
		}
		if f.cache != nil {
			if user := f.cache.GetUser(id); user != nil {
				name := user.DisplayName
				if name == "" {
					name = user.Name
				}
				return fmt.Sprintf("@%s", name)
			}
		}
		if label != "" {
			return "@" + label
		}
		return fmt.Sprintf("@%s", id)
	})
}
func (f *Formatter) resolveGroupMentions(text string) string {
	text = reSubteamLabel.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSubteamLabel.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		groupID := parts[1]
		label := parts[2]
		if f.cache != nil && (label == groupID || strings.HasPrefix(label, "S")) {
			if group := f.cache.GetUserGroup(groupID); group != nil {
				return "@" + group.Handle
			}
		}
		return "@" + label
	})
	text = reSubteamNoLabel.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSubteamNoLabel.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		groupID := parts[1]
		if f.cache != nil {
			if group := f.cache.GetUserGroup(groupID); group != nil {
				return "@" + group.Handle
			}
		}
		return "@" + groupID
	})
	text = reTeamMention.ReplaceAllString(text, "@team")
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
	for _, m := range reBareURL.FindAllString(text, -1) {
		if !seen[m] {
			urls = append(urls, m)
			seen[m] = true
		}
	}
	return urls
}

// tokenize walks the (already de-referenced) text and emits styled spans.
// It handles, in priority order: code blocks, inline code, <url|label> and
// <url> link forms, bare http(s) URLs, *bold*, _italic_, ~strike~, and
// environment / alert-status keyword highlights.
func (f *Formatter) tokenize(text string) []Span {
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
		f.appendInline(&out, text[pos:start])
		body := reCodeBlock.FindStringSubmatch(text[start:end])
		if len(body) >= 2 {
			emit(body[1], StyleCodeBlock, "")
		}
		pos = end
	}
	f.appendInline(&out, text[pos:])
	return out
}

// appendInline tokenizes a text run that has no fenced code blocks, then
// appends the resulting spans onto out.
func (f *Formatter) appendInline(out *[]Span, text string) {
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
		if m := reUserMention.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			id := text[pos+m[2] : pos+m[3]]
			label := "@" + id
			if m[4] != -1 && m[5] != -1 {
				label = "@" + text[pos+m[4]+1 : pos+m[5]]
			}

			if f.cache != nil {
				if strings.HasPrefix(id, "S") {
					if group := f.cache.GetUserGroup(id); group != nil {
						label = "@" + group.Handle
					}
				} else if user := f.cache.GetUser(id); user != nil {
					name := user.DisplayName
					if name == "" {
						name = user.Name
					}
					label = "@" + name
				}
			}
			try(cand{s, e, func() { emit(label, StyleMention, "") }})
		}
		if m := reTeamMention.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			try(cand{s, e, func() { emit("@team", StyleMention, "") }})
		}
		if m := reSubteamLabel.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			groupID := text[pos+m[2] : pos+m[3]]
			label := text[pos+m[4] : pos+m[5]]
			if f.cache != nil && (label == groupID || strings.HasPrefix(label, "S")) {
				if group := f.cache.GetUserGroup(groupID); group != nil {
					label = group.Handle
				}
			}
			try(cand{s, e, func() { emit("@"+label, StyleMention, "") }})
		}
		if m := reSubteamNoLabel.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			groupID := text[pos+m[2] : pos+m[3]]
			label := "@" + groupID
			if f.cache != nil {
				if group := f.cache.GetUserGroup(groupID); group != nil {
					label = "@" + group.Handle
				}
			}
			try(cand{s, e, func() { emit(label, StyleMention, "") }})
		}
		if m := reSpecialMention.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			label := "@" + text[pos+m[2]:pos+m[3]]
			try(cand{s, e, func() { emit(label, StyleMention, "") }})
		}
		if m := reChannelLink.FindStringSubmatchIndex(text[pos:]); m != nil {
			s, e := pos+m[0], pos+m[1]
			label := "#" + text[pos+m[4]:pos+m[5]]
			try(cand{s, e, func() { emit(label, StyleChannel, "") }})
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
	m := map[string]string{
		"thumbsup":              "\U0001F44D",
		"+1":                    "\U0001F44D",
		"thumbsdown":            "\U0001F44E",
		"-1":                    "\U0001F44E",
		"facepalm":              "\U0001F926",
		"shrug":                 "\U0001F937",
		"tada":                  "\U0001F389",
		"100":                   "\U0001F4AF",
		"100pct":                "\U0001F4AF",
		"slightly_smiling_face": "\U0001F642",
		"upside_down_face":      "\U0001F643",
	}

	for k, v := range emoji.CodeMap() {
		name := strings.Trim(k, ":")
		if _, ok := m[name]; !ok {
			m[name] = v
		}
	}

	return m
}
