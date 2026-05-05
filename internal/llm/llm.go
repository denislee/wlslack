package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var urlRegex = regexp.MustCompile(`https?://[^\s<>"]+`)

func ExtractURLs(text string) []string {
	return urlRegex.FindAllString(text, -1)
}

func fetchHTMLTitleAndDescription(link string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // read up to 1MB
	if err != nil {
		return "", err
	}

	htmlStr := string(body)

	// Very basic extraction of title and meta description
	var title string
	if start := strings.Index(strings.ToLower(htmlStr), "<title>"); start != -1 {
		if end := strings.Index(strings.ToLower(htmlStr[start:]), "</title>"); end != -1 {
			title = htmlStr[start+7 : start+end]
		}
	}

	return title, nil
}

func SummarizeLink(ctx context.Context, text string, link string) (string, error) {
	// First fetch some context from the link
	pageContext, _ := fetchHTMLTitleAndDescription(link)

	prompt := fmt.Sprintf("I have a link: %s\nThe page title might be: %s\n\nPlease write a short text telling about what this link is about. Provide a context to make people be interested in clicking on it. Do not include introductory phrases, just give the short context text.", link, pageContext)

	u := "https://text.pollinations.ai/"
	reqBody, _ := json.Marshal(map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"model": "openai",
	})

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", u, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm api status %d", resp.StatusCode)
	}

	ans, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	resText := strings.TrimSpace(string(ans))
	if resText == "" {
		return "", fmt.Errorf("empty response")
	}

	resText = strings.ToLower(resText)
	resText = regexp.MustCompile(`\bi(?:\b|(?:'))`).ReplaceAllStringFunc(resText, func(m string) string {
		return "I" + m[1:]
	})

	linkIdx := strings.Index(text, link)
	if linkIdx != -1 {
		insertPos := linkIdx + len(link)
		return text[:insertPos] + "\n\n" + resText + "\n" + text[insertPos:], nil
	}

	return text + "\n\n" + resText, nil
}
