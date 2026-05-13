package slack

import (
	"reflect"
	"testing"
)

func TestFormatSpans_Mentions(t *testing.T) {
	f := NewFormatter(nil, "15:04")
	tests := []struct {
		input    string
		expected []Span
	}{
		{
			input: "Hello <@U123>!",
			expected: []Span{
				{Text: "Hello ", Style: 0},
				{Text: "@U123", Style: StyleMention},
				{Text: "!", Style: 0},
			},
		},
		{
			input: "Hey <!here>, look at this!",
			expected: []Span{
				{Text: "Hey ", Style: 0},
				{Text: "@here", Style: StyleMention},
				{Text: ", look at this!", Style: 0},
			},
		},
		{
			input: "Check <#C123|general>.",
			expected: []Span{
				{Text: "Check ", Style: 0},
				{Text: "#general", Style: StyleChannel},
				{Text: ".", Style: 0},
			},
		},
		{
			input: "Hello <@S123|Marketing Group>!",
			expected: []Span{
				{Text: "Hello ", Style: 0},
				{Text: "@Marketing Group", Style: StyleMention},
				{Text: "!", Style: 0},
			},
		},
		{
			input: "Hello <@U123|user-name>!",
			expected: []Span{
				{Text: "Hello ", Style: 0},
				{Text: "@user-name", Style: StyleMention},
				{Text: "!", Style: 0},
			},
		},
		{
			input: "see <https://example.com|*Eng Weekly*> tomorrow",
			expected: []Span{
				{Text: "see ", Style: 0},
				{Text: "Eng Weekly", Style: StyleLink | StyleBold, Link: "https://example.com"},
				{Text: " tomorrow", Style: 0},
			},
		},
		{
			input: "<https://example.com|plain label>",
			expected: []Span{
				{Text: "plain label", Style: StyleLink, Link: "https://example.com"},
			},
		},
	}

	for _, tt := range tests {
		got := f.FormatSpans(tt.input)
		if !reflect.DeepEqual(got, tt.expected) {
			t.Errorf("FormatSpans(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}
