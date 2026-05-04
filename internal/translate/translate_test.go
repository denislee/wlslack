package translate

import "testing"

func TestNormalizeCase(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello World", "hello world"},
		{"i am here", "I am here"},
		{"i'm here", "I'm here"},
		{"<@U08BA3G4PQC> is here", "<@U08BA3G4PQC> is here"},
		{"check <#C12345> and <@W67890|Name>", "check <#C12345> and <@W67890|name>"},
		{"group <!subteam^S12345|@devs> is here", "group <!subteam^S12345|@devs> is here"},
		{"group <!subteam^S54321> check", "group <!subteam^S54321> check"},
	}

	for _, tt := range tests {
		got := normalizeCase(tt.input)
		if got != tt.expected {
			t.Errorf("normalizeCase(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
