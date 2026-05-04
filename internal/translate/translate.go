package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// pronounI matches a standalone English pronoun "i" — either alone or as the
// start of a contraction like "i'm", "i'll", "i've", "i'd". We restore the
// capital after lowercasing the rest of the translated text.
var pronounI = regexp.MustCompile(`\bi(?:\b|(?:'))`)

// ToEnglish translates text to English using the unofficial Google Translate
// endpoint. Source language is auto-detected.
func ToEnglish(ctx context.Context, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}
	endpoint := "https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl=en&dt=t&q=" + url.QueryEscape(text)

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 wlslack")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("translate: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var raw []any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("translate: empty response")
	}
	chunks, ok := raw[0].([]any)
	if !ok {
		return "", fmt.Errorf("translate: bad response shape")
	}

	var out strings.Builder
	for _, c := range chunks {
		arr, ok := c.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		s, _ := arr[0].(string)
		out.WriteString(s)
	}
	return normalizeCase(out.String()), nil
}

// normalizeCase lowercases the translated text but restores the English
// pronoun "I" (including its contractions: I'm, I'll, I've, I'd).
func normalizeCase(s string) string {
	lowered := strings.ToLower(s)
	return pronounI.ReplaceAllStringFunc(lowered, func(m string) string {
		return "I" + m[1:]
	})
}
