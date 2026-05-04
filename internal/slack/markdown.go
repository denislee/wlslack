package slack

import (
	"regexp"
	"strings"
)

var (
	reMdCodeBlock = regexp.MustCompile("(?s)```.+?```")
	reMdInlineCode = regexp.MustCompile("(?U)`[^`]+`")
	reMdLink       = regexp.MustCompile(`(?U)\[(.+)\]\((.+)\)`)
	reMdBold1      = regexp.MustCompile(`(?U)\*\*(.+)\*\*`)
	reMdBold2      = regexp.MustCompile(`(?U)__(.+)__`)
	reMdItalic     = regexp.MustCompile(`(?U)\*(.+)\*`)
	reMdStrike     = regexp.MustCompile(`(?U)~~(.+)~~`)
	reMdList       = regexp.MustCompile(`(?m)^([ \t]*)[*+-][ \t]+`)
)

// MarkdownToMrkdwn converts standard Markdown to Slack's mrkdwn format.
func MarkdownToMrkdwn(text string) string {
	// 0. Collapse double line breaks
	text = collapseBlankLines(text)

	// 1. Protect code blocks and inline code
	var codeBlocks []string
	text = reMdCodeBlock.ReplaceAllStringFunc(text, func(m string) string {
		codeBlocks = append(codeBlocks, m)
		return "\x02"
	})
	var inlineCode []string
	text = reMdInlineCode.ReplaceAllStringFunc(text, func(m string) string {
		inlineCode = append(inlineCode, m)
		return "\x03"
	})

	// 1. Escape Slack special characters: &, <, >
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	text = strings.ReplaceAll(text, ">", "&gt;")

	// 2. Lists: * item -> • item
	text = reMdList.ReplaceAllString(text, "${1}• ")

	// 3. Links: [text](url) -> <url|text>
	text = reMdLink.ReplaceAllString(text, "<${2}|${1}>")

	// 4. Bold: **bold** or __bold__ -> \x01bold\x01 (temporary)
	text = reMdBold1.ReplaceAllString(text, "\x01${1}\x01")
	text = reMdBold2.ReplaceAllString(text, "\x01${1}\x01")

	// 5. Strike: ~~strike~~ -> ~strike~
	text = reMdStrike.ReplaceAllString(text, "~${1}~")

	// 6. Italic: *italic* -> _italic_
	text = reMdItalic.ReplaceAllString(text, "_${1}_")

	// 7. Convert temporary bold markers to Slack's *
	text = strings.ReplaceAll(text, "\x01", "*")

	// 8. Restore code blocks and inline code (they must be restored in order)
	for _, c := range inlineCode {
		text = strings.Replace(text, "\x03", c, 1)
	}
	for _, c := range codeBlocks {
		text = strings.Replace(text, "\x02", c, 1)
	}

	return text
}
