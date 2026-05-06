package slack

import "testing"

func TestMarkdownToMrkdwn(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain text", "plain text"},
		{"**bold**", "*bold*"},
		{"__bold__", "*bold*"},
		{"~~strike~~", "~strike~"},
		{"[link](https://example.com)", "<https://example.com|link>"},
		{"*italic*", "_italic_"},
		{"**bold** and *italic*", "*bold* and _italic_"},
		{"nested [link](http://foo) and **bold**", "nested <http://foo|link> and *bold*"},
		{"* Item 1\n* Item 2", "• Item 1\n• Item 2"},
		{"  - Subitem", "  • Subitem"},
		{"+ Plus item", "• Plus item"},
		{"1 < 2 & 3 > 4", "1 &lt; 2 &amp; 3 &gt; 4"},
		{"[link](http://a?b&c)", "<http://a?b&amp;c|link>"},
		{"<@U12345678>", "<@U12345678>"},
		{"<!subteam^S0732MA48F2>", "<!subteam^S0732MA48F2>"},
		{"<!subteam^S0732MA48F2|@group>", "<!subteam^S0732MA48F2|@group>"},
		{"<#C12345678>", "<#C12345678>"},
		{"<!here>", "<!here>"},
		{"<!team^T12345>", "<!team^T12345>"},
		{"`*not italic*`", "`*not italic*`"},
		{"```\n**not bold**\n```", "```\n**not bold**\n```"},
		{"Line 1\n\nLine 2", "Line 1\nLine 2"},
		{"Line 1\n \n \nLine 2", "Line 1\nLine 2"},
	}

	for _, tt := range tests {
		got := MarkdownToMrkdwn(tt.in)
		if got != tt.want {
			t.Errorf("MarkdownToMrkdwn(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
