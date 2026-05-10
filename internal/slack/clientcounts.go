package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// ChannelUnreadState is the per-conversation unread snapshot returned by
// the aggregate-counts endpoints (client.counts / users.counts). Latest and
// LastRead are Slack message timestamps; HasUnreads reflects Slack's own
// "is this channel unread" determination, which we treat as authoritative
// since it accounts for muted threads, channel mute settings, etc. that
// conversations.info doesn't.
type ChannelUnreadState struct {
	ID           string
	LastRead     string
	Latest       string
	HasUnreads   bool
	MentionCount int
}

var errCountsUnsupported = errors.New("aggregate counts endpoint not supported for this token")

// IsClientCountsSupported reports whether the fast aggregate-unreads path is
// available. xoxc tokens hit client.counts (workspace `d` cookie required);
// xoxp tokens fall back to users.counts (deprecated but still works for many
// workspaces); xoxb tokens have no aggregate endpoint and use per-channel
// conversations.info.
func (c *Client) IsClientCountsSupported() bool {
	if c.aggregateCountsDisabled.Load() {
		return false
	}
	tokenPrefix := ""
	if len(c.token) >= 5 {
		tokenPrefix = c.token[:5]
	}
	supported := false
	switch {
	case strings.HasPrefix(c.token, "xoxc-") && c.cookie != "":
		supported = true
	case strings.HasPrefix(c.token, "xoxp-"):
		supported = true
	}
	slog.Debug("aggregate counts support check",
		"token_prefix", tokenPrefix,
		"has_cookie", c.cookie != "",
		"supported", supported)
	return supported
}

// isTerminalCountsError reports whether the Slack error code means we should
// stop calling the aggregate-counts endpoint for the rest of this session.
// Transient errors (rate_limited, network) keep the endpoint enabled so the
// next tick retries.
func isTerminalCountsError(slackErr string) bool {
	switch slackErr {
	case "method_unknown", "method_deprecated", "missing_scope",
		"not_allowed_token_type", "invalid_auth", "account_inactive",
		"token_revoked", "no_permission":
		return true
	}
	return false
}

// GetClientCounts fetches unread/mention state for every conversation the
// signed-in user can see, in a single request. xoxc tokens go through
// client.counts (the endpoint Slack's own web client uses); xoxp tokens go
// through users.counts (deprecated but still available on most workspaces).
// Either replaces the per-channel conversations.info polling that's bound by
// Tier 3 rate limits and can take minutes to cycle through a large workspace.
func (c *Client) GetClientCounts() ([]ChannelUnreadState, error) {
	switch {
	case strings.HasPrefix(c.token, "xoxc-") && c.cookie != "":
		return c.fetchAggregateCounts("client.counts")
	case strings.HasPrefix(c.token, "xoxp-"):
		return c.fetchAggregateCounts("users.counts")
	default:
		return nil, errCountsUnsupported
	}
}

// fetchAggregateCounts is the shared HTTP path for client.counts and
// users.counts. The two endpoints accept the same form params and return the
// same channels/ims/mpims shape, so we route both through one function.
func (c *Client) fetchAggregateCounts(method string) ([]ChannelUnreadState, error) {
	form := url.Values{}
	form.Set("token", c.token)
	form.Set("simple_unreads", "true")
	form.Set("include_threads", "true")
	form.Set("mpim_aware", "true")

	req, err := http.NewRequest("POST", "https://slack.com/api/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")

	httpClient := newSlackHTTPClient(c.cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s read: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("%s: HTTP %d body=%q", method, resp.StatusCode, preview)
	}

	type entry struct {
		ID                  string `json:"id"`
		LastRead            string `json:"last_read"`
		Latest              string `json:"latest"`
		HasUnreads          bool   `json:"has_unreads"`
		MentionCount        int    `json:"mention_count"`
		MentionCountDisplay int    `json:"mention_count_display"`
	}
	var out struct {
		OK       bool    `json:"ok"`
		Error    string  `json:"error,omitempty"`
		Channels []entry `json:"channels"`
		Groups   []entry `json:"groups"`
		MPIMs    []entry `json:"mpims"`
		IMs      []entry `json:"ims"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("%s decode: %w body=%q", method, err, preview)
	}
	if !out.OK {
		if isTerminalCountsError(out.Error) {
			c.aggregateCountsDisabled.Store(true)
			slog.Warn("aggregate counts endpoint disabled for session",
				"method", method,
				"error", out.Error)
		}
		return nil, fmt.Errorf("%s: %s", method, out.Error)
	}

	withUnreads := 0
	all := make([][]entry, 0, 4)
	all = append(all, out.Channels, out.Groups, out.MPIMs, out.IMs)
	for _, src := range all {
		for _, e := range src {
			if e.HasUnreads {
				withUnreads++
			}
		}
	}
	slog.Info(method+" ok",
		"channels", len(out.Channels),
		"groups", len(out.Groups),
		"ims", len(out.IMs),
		"mpims", len(out.MPIMs),
		"with_unreads", withUnreads)

	counts := make([]ChannelUnreadState, 0, len(out.Channels)+len(out.Groups)+len(out.IMs)+len(out.MPIMs))
	for _, src := range all {
		for _, e := range src {
			mentions := e.MentionCountDisplay
			if mentions == 0 {
				mentions = e.MentionCount
			}
			counts = append(counts, ChannelUnreadState{
				ID:           e.ID,
				LastRead:     e.LastRead,
				Latest:       e.Latest,
				HasUnreads:   e.HasUnreads,
				MentionCount: mentions,
			})
		}
	}
	return counts, nil
}

// ApplyClientCounts updates cached unread state for each entry. It uses the
// existing SetChannelUnread path so monotonicity guards and the OnUpdate
// callback fire normally; channels not yet known to the cache are skipped (the
// next conversations.list refresh will pick them up).
func (c *Client) ApplyClientCounts(counts []ChannelUnreadState) {
	for _, st := range counts {
		cached := c.cache.GetChannel(st.ID)
		if cached == nil {
			continue
		}

		latest := st.Latest
		if latest == "" || latest < cached.LatestTS {
			latest = cached.LatestTS
		}

		unread := 0
		if st.HasUnreads || (latest != "" && latest > st.LastRead) {
			// The aggregate endpoints don't return a numeric unread count;
			// treat the presence of unreads as "1+" and let the per-channel
			// mention scan populate the precise mention number.
			unread = max(cached.UnreadCount, 1)
		}

		mentions := st.MentionCount
		if mentions == 0 && unread > 0 && cached.MentionCount > 0 {
			// Preserve the locally-scanned mention count until the next scan
			// confirms it; otherwise the badge would briefly drop on every
			// refresh tick.
			mentions = cached.MentionCount
		}

		c.cache.SetChannelUnread(st.ID, unread, mentions, st.LastRead, latest)
	}
}
