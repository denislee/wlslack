package slack

import (
	"testing"

	"github.com/slack-go/slack"
)

func TestExtractAttachmentText(t *testing.T) {
	c := &Client{}

	tests := []struct {
		name        string
		attachments []slack.Attachment
		expected    string
	}{
		{
			name: "Fallback text only",
			attachments: []slack.Attachment{
				{
					Fallback: "This is fallback text",
				},
			},
			expected: "This is fallback text",
		},
		{
			name: "Pretext, Title, Text",
			attachments: []slack.Attachment{
				{
					Pretext: "Pretext",
					Title:   "Title",
					Text:    "Text",
					Fallback: "Fallback should be ignored if other fields are present",
				},
			},
			expected: "Pretext\nTitle\nText",
		},
		{
			name: "With Blocks",
			attachments: []slack.Attachment{
				{
					Fallback: "Fallback text",
					Blocks: slack.Blocks{
						BlockSet: []slack.Block{
							slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", "Block text", false, false), nil, nil),
						},
					},
				},
			},
			expected: "Block text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.extractAttachmentText(tt.attachments)
			if got != tt.expected {
				t.Errorf("extractAttachmentText() = %v, want %v", got, tt.expected)
			}
		})
	}
}