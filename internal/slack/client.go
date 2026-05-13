package slack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	slackapi "github.com/slack-go/slack"
)

// cookieTransport attaches a `d=<value>` cookie on every request to slack.com
// hosts. Slack's xoxc browser-session tokens are validated against the d cookie
// in addition to the Authorization header — without this, files.slack.com 302s
// authenticated requests to the workspace web login page.
type cookieTransport struct {
	base   http.RoundTripper
	cookie string
}

func (t *cookieTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.cookie != "" && (strings.HasSuffix(req.URL.Host, ".slack.com") || req.URL.Host == "slack.com") {
		// Clone before mutating: the http.Client may retry the same Request.
		r2 := req.Clone(req.Context())
		r2.AddCookie(&http.Cookie{Name: "d", Value: t.cookie})
		req = r2
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// newSlackHTTPClient returns an http.Client whose transport attaches the d
// cookie when one is configured. When cookie is empty, it returns a vanilla
// client so xoxb/xoxp tokens see no behavior change.
func newSlackHTTPClient(cookie string) *http.Client {
	if cookie == "" {
		return &http.Client{}
	}
	return &http.Client{Transport: &cookieTransport{cookie: cookie}}
}

var slackErrors = map[string]string{
	"restricted_action_read_only_channel": "This channel is read-only",
	"restricted_action":                   "You don't have permission to do this",
	"channel_not_found":                   "Channel not found",
	"not_in_channel":                      "You're not in this channel",
	"is_archived":                         "This channel is archived",
	"msg_too_long":                        "Message is too long",
	"no_text":                             "Message cannot be empty",
	"rate_limited":                        "Rate limited — try again shortly",
	"invalid_auth":                        "Invalid auth token",
	"account_inactive":                    "Account is inactive",
	"token_revoked":                       "Token has been revoked",
	"not_authed":                          "Not authenticated",
	"already_reacted":                     "You already reacted with this emoji",
	"no_reaction":                         "You haven't reacted with this emoji",
	"too_many_reactions":                  "Too many reactions on this message",
}

func (c *Client) GetEmoji() (map[string]string, error) {
	return c.api.GetEmoji()
}

func (c *Client) MergeMessages(a, b []Message) []Message {
	if len(b) == 0 {
		return a
	}

	// Determine the time range covered by the new messages.
	minTS, maxTS := b[0].Timestamp, b[0].Timestamp
	bMap := make(map[string]bool, len(b))
	for _, msg := range b {
		if msg.Timestamp < minTS {
			minTS = msg.Timestamp
		}
		if msg.Timestamp > maxTS {
			maxTS = msg.Timestamp
		}
		bMap[msg.Timestamp] = true
	}

	merged := make([]Message, 0, len(a)+len(b))
	i, j := 0, 0

	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			existing := a[i]
			if existing.Timestamp >= minTS && existing.Timestamp <= maxTS && !existing.Deleted && !bMap[existing.Timestamp] {
				existing.Deleted = true
			}
			merged = append(merged, existing)
			i++
		} else if a[i].Timestamp > b[j].Timestamp {
			merged = append(merged, b[j])
			j++
		} else {
			existing := a[i]
			msg := b[j]

			msg.EditHistory = mergeHistory(existing.EditHistory, msg.EditHistory)

			if msg.Edited && msg.EditedTS != "" && existing.Text != msg.Text {
				prevTS := existing.EditedTS
				if prevTS == "" {
					prevTS = existing.Timestamp
				}
				if !hasEdit(msg.EditHistory, prevTS) {
					msg.EditHistory = append(msg.EditHistory, Edit{
						Timestamp: prevTS,
						Text:      existing.Text,
					})
				}
			} else if existing.Text == msg.Text && existing.EditHistory != nil && msg.EditHistory == nil {
				msg.EditHistory = existing.EditHistory
			}

			merged = append(merged, msg)
			i++
			j++
		}
	}

	for i < len(a) {
		existing := a[i]
		if existing.Timestamp >= minTS && existing.Timestamp <= maxTS && !existing.Deleted && !bMap[existing.Timestamp] {
			existing.Deleted = true
		}
		merged = append(merged, existing)
		i++
	}

	for j < len(b) {
		merged = append(merged, b[j])
		j++
	}

	return merged
}

func mergeHistory(a, b []Edit) []Edit {
	seen := make(map[Edit]bool, len(a)+len(b))
	merged := make([]Edit, 0, len(a)+len(b))
	for _, e := range a {
		if !seen[e] {
			merged = append(merged, e)
			seen[e] = true
		}
	}
	for _, e := range b {
		if !seen[e] {
			merged = append(merged, e)
			seen[e] = true
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp < merged[j].Timestamp
	})
	return merged
}

func hasEdit(history []Edit, ts string) bool {
	for _, e := range history {
		if e.Timestamp == ts {
			return true
		}
	}
	return false
}

func friendlyError(err error) error {
	var slackErr slackapi.SlackErrorResponse
	if errors.As(err, &slackErr) {
		if msg, ok := slackErrors[slackErr.Err]; ok {
			return fmt.Errorf("%s", msg)
		}
	}
	errStr := err.Error()
	if msg, ok := slackErrors[errStr]; ok {
		return fmt.Errorf("%s", msg)
	}
	return err
}

type Client struct {
	api    *slackapi.Client
	cache  *Cache
	selfID string
	token  string
	cookie string

	workspaceURL string

	disableLinkUnfurl  bool
	disableMediaUnfurl bool

	// aggregateCountsDisabled is set the first time users.counts /
	// client.counts returns a terminal error (method_unknown, method_deprecated,
	// missing_scope, etc.) so we stop hammering the endpoint and let the
	// per-channel fallback take over for the rest of the session.
	aggregateCountsDisabled atomic.Bool
}

func NewClient(token, cookie string) (*Client, error) {
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}
	var opts []slackapi.Option
	if cookie != "" {
		opts = append(opts, slackapi.OptionHTTPClient(newSlackHTTPClient(cookie)))
	}
	api := slackapi.New(token, opts...)
	c := &Client{
		api:    api,
		cache:  NewCache(),
		token:  token,
		cookie: cookie,
	}
	channelNameLookup = func(id string) (string, bool) {
		if ch := c.cache.GetChannel(id); ch != nil && ch.Name != "" {
			return ch.Name, true
		}
		return "", false
	}
	return c, nil
}

func (c *Client) SetUnfurlSettings(links, media bool) {
	c.disableLinkUnfurl = links
	c.disableMediaUnfurl = media
}

func (c *Client) AuthTest() (string, error) {
	resp, err := c.api.AuthTest()
	if err != nil {
		return "", fmt.Errorf("auth test failed: %w", err)
	}
	c.workspaceURL = resp.URL
	return resp.UserID, nil
}

func (c *Client) Permalink(channelID, timestamp string) string {
	if c.workspaceURL == "" || channelID == "" || timestamp == "" {
		return ""
	}
	// Permalinks are https://{domain}.slack.com/archives/{channelID}/p{timestampWithoutDot}
	ts := strings.ReplaceAll(timestamp, ".", "")
	url := strings.TrimSuffix(c.workspaceURL, "/")
	return fmt.Sprintf("%s/archives/%s/p%s", url, channelID, ts)
}

// VerifyFileAccess pokes files.list with a tiny page size so we can tell
// "token lacks files:read scope" apart from "files.slack.com is rejecting the
// CDN auth handshake" — both look identical in the image-loader logs.
// Returns a typed error so callers can format actionable advice.
func (c *Client) VerifyFileAccess() error {
	var errs []string

	// Probe files:read
	_, _, err := c.api.ListFiles(slackapi.ListFilesParameters{Limit: 1})
	if err != nil {
		var serr slackapi.SlackErrorResponse
		if (errors.As(err, &serr) && serr.Err == "missing_scope") || strings.Contains(err.Error(), "missing_scope") {
			errs = append(errs, "token lacks files:read scope: regenerate the token with files:read (and files:write if you want uploads)")
		} else {
			slog.Debug("files.list probe failed", "error", err)
		}
	}

	// Probe usergroups:read
	_, err = c.api.GetUserGroups()
	if err != nil {
		var serr slackapi.SlackErrorResponse
		if (errors.As(err, &serr) && serr.Err == "missing_scope") || strings.Contains(err.Error(), "missing_scope") {
			errs = append(errs, "token lacks usergroups:read scope: group mentions will show as IDs instead of names")
		} else {
			slog.Debug("usergroups.list probe failed", "error", err)
		}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}

func (c *Client) SetSelfID(id string) {
	c.selfID = id
}

func (c *Client) GetSelfID() string {
	return c.selfID
}

func (c *Client) Cache() *Cache {
	return c.cache
}

func (c *Client) GetChannels(types []string, priorityIDs []string) ([]Channel, error) {
	slog.Debug("GetChannels starting", "types", types, "priority", len(priorityIDs))
	var allChannels []Channel
	cursor := ""
	page := 0

	priorityMap := make(map[string]bool)
	for _, id := range priorityIDs {
		priorityMap[id] = true
	}

	for {
		page++
		params := &slackapi.GetConversationsParameters{
			Types:           types,
			ExcludeArchived: true,
			Limit:           200,
			Cursor:          cursor,
		}
		channels, nextCursor, err := c.api.GetConversations(params)
		if err != nil {
			slog.Error("GetChannels API error", "page", page, "error", err)
			return nil, fmt.Errorf("get conversations: %w", err)
		}
		slog.Debug("GetChannels page fetched", "page", page, "count", len(channels))

		for _, ch := range channels {
			cc := Channel{
				ID:         ch.ID,
				Name:       ch.Name,
				IsIM:       ch.IsIM,
				IsMPIM:     ch.IsMpIM,
				IsPrivate:  ch.IsPrivate,
				IsExternal: ch.IsExtShared || ch.IsOrgShared,
				Topic:      ch.Topic.Value,
				Purpose:    ch.Purpose.Value,
			}
			if ch.IsIM {
				cc.UserID = ch.User
				if u := c.cache.GetUser(ch.User); u != nil {
					cc.Name = imDisplayName(u, ch.User)
				} else {
					cc.Name = ch.User
				}
			}

			// Pre-populate from cache so we have data for hideEmpty even if enrichment is skipped.
			activityChanged := false
			if cached := c.cache.GetChannel(ch.ID); cached != nil {
				if (ch.IsMpIM || ch.IsIM) && cached.Name != "" && !strings.HasPrefix(cached.Name, "mpdm-") && !strings.HasPrefix(cached.Name, "U") {
					cc.Name = cached.Name
				}
				cc.UnreadCount = cached.UnreadCount
				cc.LatestTS = cached.LatestTS
				cc.LastReadTS = cached.LastReadTS
				if cc.LatestTS != "" {
					cc.LatestTSVerified = true
				}
			}

			if ch.Latest != nil {
				if ch.Latest.Timestamp > cc.LatestTS {
					activityChanged = true
					cc.LatestTS = ch.Latest.Timestamp
				}
				cc.LatestTSVerified = true
			}

			// Optimistic unread detection: if LatestTS > LastReadTS, we have unreads.
			// This allows the UI to show activity immediately after the list fetch
			// without waiting for the slow GetConversationInfo enrichment.
			// We MUST NOT do this if LastReadTS is equal to or greater than LatestTS,
			// as that would resurge unreads after they were marked read locally.
			if cc.LatestTS != "" && cc.LatestTS > cc.LastReadTS && cc.UnreadCount == 0 {
				cc.UnreadCount = 1
			} else if cc.LatestTS != "" && cc.LatestTS <= cc.LastReadTS {
				// Ensure unread count is cleared if we've caught up.
				cc.UnreadCount = 0
			}

			if activityChanged {
				priorityMap[cc.ID] = true
			}

			allChannels = append(allChannels, cc)
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	slog.Info("GetChannels list fetched", "total", len(allChannels), "pages", page)

	// Enrich only priority channels or those with newly discovered activity.
	// This keeps us well within Tier 3 rate limits (50+ calls/min) even with
	// frequent polling of the conversation list.
	toEnrich := make([]string, 0)
	for id := range priorityMap {
		toEnrich = append(toEnrich, id)
	}
	c.enrichWithUnreadCounts(allChannels, toEnrich)

	unreadCount := 0
	for _, ch := range allChannels {
		if ch.UnreadCount > 0 {
			unreadCount++
		}
	}
	slog.Info("GetChannels done", "total", len(allChannels), "with_unread", unreadCount)

	c.cache.SetChannels(allChannels)
	_ = c.cache.SaveChannelsToDisk(c.cache.GetAllChannels())
	return allChannels, nil
}

// imDisplayName picks the best label for a 1:1 DM partner, falling back to the
// raw user ID if neither display nor real name is set.
func imDisplayName(u *User, fallbackID string) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Name != "" {
		return u.Name
	}
	return fallbackID
}

// ResolveConversationNames fills in display names for IM and MPIM channels in
// parallel, using the per-user resolution cache. It mutates channels in place,
// then refreshes the in-memory cache and on-disk snapshot. Safe to call from a
// goroutine after GetChannels has returned.
func (c *Client) ResolveConversationNames(channels []Channel) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for i := range channels {
		if channels[i].IsIM && channels[i].UserID != "" {
			if u := c.cache.GetUser(channels[i].UserID); u != nil {
				channels[i].Name = imDisplayName(u, channels[i].UserID)
				continue
			}
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				user, err := c.ResolveUser(channels[idx].UserID)
				if err != nil {
					slog.Debug("ResolveConversationNames: resolve failed", "user", channels[idx].UserID, "error", err)
					return
				}
				channels[idx].Name = imDisplayName(user, channels[idx].UserID)
			}(i)
		} else if channels[i].IsMPIM {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				memberIDs, _, err := c.api.GetUsersInConversation(&slackapi.GetUsersInConversationParameters{
					ChannelID: channels[idx].ID,
				})
				if err != nil {
					slog.Debug("ResolveConversationNames: get members failed", "channel", channels[idx].ID, "error", err)
					return
				}

				var names []string
				for _, uid := range memberIDs {
					if uid == c.selfID {
						continue
					}
					user, err := c.ResolveUser(uid)
					if err != nil {
						names = append(names, uid)
						continue
					}
					names = append(names, imDisplayName(user, uid))
				}
				sort.Strings(names)
				if len(names) > 0 {
					channels[idx].Name = strings.Join(names, ", ")
				}
			}(i)
		}
	}
	wg.Wait()

	c.cache.SetChannels(channels)
	_ = c.cache.SaveChannelsToDisk(channels)
}

func (c *Client) GetUnreadCounts(ids []string) ([]Channel, error) {
	channels := make([]Channel, 0, len(ids))
	for _, id := range ids {
		ch := c.cache.GetChannel(id)
		if ch != nil {
			channels = append(channels, *ch)
		} else {
			channels = append(channels, Channel{ID: id})
		}
	}

	c.enrichWithUnreadCounts(channels, ids)

	for _, ch := range channels {
		c.cache.SetChannelUnread(ch.ID, ch.UnreadCount, ch.MentionCount, ch.LastReadTS, ch.LatestTS)
	}

	return channels, nil
}

// getConversationInfoWithRetry calls conversations.info and waits out a single
// rate-limit response before retrying. Without this, a brief Tier 3 burst (e.g.
// after the periodic full scan) would leave a channel's unread state stale
// until the next polling cycle — favorites in particular would silently miss
// their badge for a window. Rate-limit errors are no longer logged at the call
// site since the retry handles them; only terminal failures surface.
func (c *Client) getConversationInfoWithRetry(channelID, channelName string) (*slackapi.Channel, error) {
	const maxRetries = 1
	for attempt := 0; ; attempt++ {
		info, err := c.api.GetConversationInfo(&slackapi.GetConversationInfoInput{
			ChannelID:         channelID,
			IncludeNumMembers: true,
		})
		if err == nil {
			return info, nil
		}
		var rl *slackapi.RateLimitedError
		if errors.As(err, &rl) && attempt < maxRetries {
			wait := rl.RetryAfter
			if wait <= 0 || wait > 30*time.Second {
				wait = 5 * time.Second
			}
			time.Sleep(wait)
			continue
		}
		if strings.Contains(err.Error(), "missing_scope") {
			slog.Warn("conversations.info failed: missing scope (needs channels:read, groups:read, im:read, or mpim:read)", "channel", channelName, "error", err)
		} else if !errors.As(err, &rl) {
			slog.Debug("conversations.info error", "channel", channelName, "error", err)
		}
		return nil, err
	}
}

func (c *Client) enrichWithUnreadCounts(channels []Channel, priorityIDs []string) {
	var wg sync.WaitGroup
	// Tier 3 (conversations.info) is ~50 req/min ≈ 0.83 req/s. Keep concurrency
	// low and stagger meaningfully so a refresh of 10–30 priority channels
	// doesn't burn the bucket and starve the next cycle. With concurrency 3
	// and 250ms post-call stagger we stay roughly within burst tolerance and
	// rate-limited calls retry once via the helper below.
	sem := make(chan struct{}, 3)

	priorityMap := make(map[string]bool)
	for _, id := range priorityIDs {
		priorityMap[id] = true
	}

	for i := range channels {
		isPriority := priorityMap[channels[i].ID] || channels[i].UnreadCount > 0
		if !isPriority {
			continue
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() {
				time.Sleep(250 * time.Millisecond)
				<-sem
			}()

			info, err := c.getConversationInfoWithRetry(channels[idx].ID, channels[idx].Name)
			if err != nil {
				return
			}

			// Capture the pre-call state so fallbacks below can preserve a
			// more precise count (e.g. from scanMentions or client.counts +
			// local scan) when conversations.info returns the imprecise
			// UnreadCountDisplay==0 that's typical of xoxc/xoxb tokens.
			prevUnread := channels[idx].UnreadCount
			prevLastRead := channels[idx].LastReadTS

			infoLatest := ""
			if info.Latest != nil {
				infoLatest = info.Latest.Timestamp
			}
			slog.Debug("conversations.info raw",
				"channel", channels[idx].Name,
				"id", channels[idx].ID,
				"unread_display", info.UnreadCountDisplay,
				"info_last_read", info.LastRead,
				"info_latest", infoLatest,
				"cache_last_read", channels[idx].LastReadTS,
				"cache_latest", channels[idx].LatestTS)
			if info.UnreadCountDisplay > 0 || (infoLatest != "" && infoLatest > info.LastRead) {
				slog.Info("conversations.info: unread detected",
					"channel", channels[idx].Name,
					"id", channels[idx].ID,
					"unread_display", info.UnreadCountDisplay,
					"info_last_read", info.LastRead,
					"info_latest", infoLatest,
					"cache_last_read", channels[idx].LastReadTS,
					"cache_latest", channels[idx].LatestTS)
			}

			// Stale info protection: Slack's conversations.info is eventually consistent.
			// If we recently marked the channel as read (e.g. locally or in another
			// client that updated our state), the info we get back might have an
			// older LastRead and thus a non-zero UnreadCount.
			if channels[idx].LastReadTS != "" && (info.LastRead == "" || info.LastRead < channels[idx].LastReadTS) {
				// We have more recent read state than Slack is reporting.
				// Keep our LastReadTS and only accept UnreadCount if it's 0 (meaning Slack is ahead or caught up).
				if info.UnreadCountDisplay == 0 {
					channels[idx].UnreadCount = 0
				} else if channels[idx].LastReadTS >= channels[idx].LatestTS {
					// We've read up to the latest we know about.
					channels[idx].UnreadCount = 0
				}
				// Otherwise, keep current UnreadCount as it's more likely to be accurate than a stale Slack report.
			} else {
				channels[idx].UnreadCount = info.UnreadCountDisplay
				channels[idx].LastReadTS = info.LastRead
			}

			if info.Latest != nil {
				if info.Latest.Timestamp > channels[idx].LatestTS {
					channels[idx].LatestTS = info.Latest.Timestamp
				}
			}

			// Fallback: if UnreadCountDisplay is 0 but LatestTS > LastReadTS,
			// assume at least 1 unread. Bot tokens often get 0 for
			// UnreadCountDisplay even when unreads exist. When LastReadTS
			// hasn't advanced, the previously cached count (set by
			// scanMentions or a prior info call) is still valid — preserve
			// it instead of clobbering a real number like 5 down to 1 every
			// refresh tick.
			if channels[idx].UnreadCount == 0 && channels[idx].LatestTS != "" {
				if channels[idx].LatestTS > channels[idx].LastReadTS {
					if channels[idx].LastReadTS == prevLastRead && prevUnread > 1 {
						channels[idx].UnreadCount = prevUnread
					} else {
						channels[idx].UnreadCount = 1
					}
				}
			}

			// History probe: conversations.info doesn't return the channel's
			// `latest` timestamp on most tokens, so UnreadCountDisplay==0 ends
			// up looking like "caught up" when there's actually new activity
			// past LastRead. We always do a limit=1 history probe (when not
			// rate-limit-blocked) to get the real latest TS and reconcile.
			//
			// To avoid doubling our Tier 3 footprint we skip the probe when
			// UnreadCountDisplay already reports unreads (the token has the
			// right scope and we trust the count).
			if channels[idx].UnreadCount == 0 {
				h, err := c.api.GetConversationHistory(&slackapi.GetConversationHistoryParameters{
					ChannelID: channels[idx].ID,
					Limit:     1,
				})
				if err == nil && len(h.Messages) > 0 {
					histTS := h.Messages[0].Timestamp
					if histTS > channels[idx].LatestTS {
						channels[idx].LatestTS = histTS
					}
					if channels[idx].LastReadTS != "" && histTS > channels[idx].LastReadTS {
						if channels[idx].LastReadTS == prevLastRead && prevUnread > 1 {
							channels[idx].UnreadCount = prevUnread
						} else {
							channels[idx].UnreadCount = 1
						}
						slog.Info("history probe: unread detected",
							"channel", channels[idx].Name,
							"id", channels[idx].ID,
							"hist_latest", histTS,
							"last_read", channels[idx].LastReadTS)
					}
				} else if err != nil {
					slog.Debug("history probe failed", "channel", channels[idx].Name, "error", err)
				}
			}
			channels[idx].LatestTSVerified = true
		}(i)
	}
	wg.Wait()
}

func (c *Client) GetMessages(channelID string, limit int, oldest string) ([]Message, error) {
	params := &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     limit,
	}
	if oldest != "" {
		params.Oldest = oldest
	}

	history, err := c.api.GetConversationHistory(params)
	if err != nil {
		return nil, fmt.Errorf("get history: %w", err)
	}

	messages := make([]Message, 0, len(history.Messages))
	for _, msg := range history.Messages {
		messages = append(messages, c.convertMessage(msg))
	}

	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	cached := c.cache.GetMessages(channelID)
	merged := c.MergeMessages(cached, messages)

	c.cache.SetMessages(channelID, merged)
	_ = c.cache.SaveMessagesToDisk(channelID, merged)
	go c.ResolveMentions(merged)
	return merged, nil
}

func (c *Client) GetMessagesContext(channelID string, limit int, ts string) ([]Message, error) {
	half := limit / 2

	paramsBefore := &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     half,
		Latest:    ts,
		Inclusive: true,
	}
	historyBefore, err := c.api.GetConversationHistory(paramsBefore)
	if err != nil {
		return nil, fmt.Errorf("history before: %w", err)
	}

	paramsAfter := &slackapi.GetConversationHistoryParameters{
		ChannelID: channelID,
		Limit:     half,
		Oldest:    ts,
		Inclusive: false,
	}
	historyAfter, err := c.api.GetConversationHistory(paramsAfter)
	if err != nil {
		return nil, fmt.Errorf("history after: %w", err)
	}

	var all []slackapi.Message
	if historyAfter != nil && len(historyAfter.Messages) > 0 {
		all = append(all, historyAfter.Messages...)
	}
	if historyBefore != nil && len(historyBefore.Messages) > 0 {
		all = append(all, historyBefore.Messages...)
	}

	messages := make([]Message, 0, len(all))
	for _, msg := range all {
		messages = append(messages, c.convertMessage(msg))
	}

	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	cached := c.cache.GetMessages(channelID)
	merged := c.MergeMessages(cached, messages)

	c.cache.SetMessages(channelID, merged)
	_ = c.cache.SaveMessagesToDisk(channelID, merged)
	go c.ResolveMentions(merged)
	return merged, nil
}

func (c *Client) GetThreadReplies(channelID, threadTS string) ([]Message, error) {
	msgs, _, _, err := c.api.GetConversationReplies(&slackapi.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
	})
	if err != nil {
		return nil, fmt.Errorf("get replies: %w", err)
	}

	replies := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		replies = append(replies, c.convertMessage(msg))
	}

	cached := c.cache.GetThread(channelID, threadTS)
	merged := c.MergeMessages(cached, replies)

	c.cache.SetThread(channelID, threadTS, merged)
	_ = c.cache.SaveThreadMessagesToDisk(channelID, threadTS, merged)
	go c.ResolveMentions(merged)
	return merged, nil
}

// ResolveMentions scans messages for user and group IDs that aren't in the
// cache and resolves them in the background.
func (c *Client) ResolveMentions(messages []Message) {
	userIDs := make(map[string]bool)
	groupIDs := make(map[string]bool)

	for _, msg := range messages {
		for _, m := range reUserMention.FindAllStringSubmatch(msg.Text, -1) {
			id := m[1]
			if strings.HasPrefix(id, "S") {
				groupIDs[id] = true
			} else {
				userIDs[id] = true
			}
		}
		for _, m := range reSubteamNoLabel.FindAllStringSubmatch(msg.Text, -1) {
			groupIDs[m[1]] = true
		}
		for _, m := range reSubteamLabel.FindAllStringSubmatch(msg.Text, -1) {
			groupIDs[m[1]] = true
		}
		for _, r := range msg.Reactions {
			for _, u := range r.Users {
				userIDs[u] = true
			}
		}
		for _, u := range msg.ReplyUsers {
			userIDs[u] = true
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for id := range userIDs {
		if id == "" || c.cache.GetUser(id) != nil {
			continue
		}
		wg.Add(1)
		go func(userID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, _ = c.ResolveUser(userID)
		}(id)
	}

	if len(groupIDs) > 0 {
		needGroups := false
		for id := range groupIDs {
			if c.cache.GetUserGroup(id) == nil {
				needGroups = true
				break
			}
		}
		if needGroups {
			if _, err := c.GetUserGroups(); err != nil {
				slog.Warn("resolve usergroups failed", "error", err)
			}
		}
	}

	wg.Wait()
}

func (c *Client) SendMessage(channelID, text string) error {
	text = MarkdownToMrkdwn(text)
	var opts []slackapi.MsgOption
	opts = append(opts, slackapi.MsgOptionText(text, false))
	if c.disableLinkUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableLinkUnfurl())
	} else {
		opts = append(opts, slackapi.MsgOptionEnableLinkUnfurl())
	}
	if c.disableMediaUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableMediaUnfurl())
	}
	_, _, err := c.api.PostMessage(
		channelID,
		opts...,
	)
	if err != nil {
		return fmt.Errorf("send message: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) UpdateMessage(channelID, timestamp, text string) error {
	text = MarkdownToMrkdwn(text)
	var opts []slackapi.MsgOption
	opts = append(opts, slackapi.MsgOptionText(text, false))
	if c.disableLinkUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableLinkUnfurl())
	} else {
		opts = append(opts, slackapi.MsgOptionEnableLinkUnfurl())
	}
	if c.disableMediaUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableMediaUnfurl())
	}
	_, _, _, err := c.api.UpdateMessage(
		channelID,
		timestamp,
		opts...,
	)
	if err != nil {
		return fmt.Errorf("update message: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) DeleteMessage(channelID, timestamp string) error {
	_, _, err := c.api.DeleteMessage(channelID, timestamp)
	if err != nil {
		return fmt.Errorf("delete message: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) SendThreadReply(channelID, threadTS, text string) error {
	text = MarkdownToMrkdwn(text)
	var opts []slackapi.MsgOption
	opts = append(opts,
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionTS(threadTS),
	)
	if c.disableLinkUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableLinkUnfurl())
	} else {
		opts = append(opts, slackapi.MsgOptionEnableLinkUnfurl())
	}
	if c.disableMediaUnfurl {
		opts = append(opts, slackapi.MsgOptionDisableMediaUnfurl())
	}
	_, _, err := c.api.PostMessage(
		channelID,
		opts...,
	)
	if err != nil {
		return fmt.Errorf("send reply: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) UploadFile(channelID, threadTS, filename string, data []byte, initialComment string, onProgress func(float32)) error {
	params := slackapi.UploadFileParameters{
		Channel:         channelID,
		ThreadTimestamp: threadTS,
		Filename:        filename,
		FileSize:        len(data),
		Reader: &progressReader{
			r:     bytes.NewReader(data),
			total: int64(len(data)),
			cb:    onProgress,
		},
		InitialComment: initialComment,
	}
	_, err := c.api.UploadFile(params)
	if err != nil {
		return fmt.Errorf("upload file: %w", friendlyError(err))
	}
	return nil
}

type progressReader struct {
	r     io.Reader
	total int64
	read  int64
	cb    func(float32)
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.r.Read(p)
	pr.read += int64(n)
	if pr.total > 0 && pr.cb != nil {
		pr.cb(float32(pr.read) / float32(pr.total))
	}
	return n, err
}

func (c *Client) AddReaction(channelID, timestamp, emoji string) error {
	ref := slackapi.ItemRef{
		Channel:   channelID,
		Timestamp: timestamp,
	}
	if err := c.api.AddReaction(emoji, ref); err != nil {
		return friendlyError(err)
	}
	return nil
}

func (c *Client) GetPermalink(channelID, timestamp string) (string, error) {
	return c.api.GetPermalink(&slackapi.PermalinkParameters{
		Channel: channelID,
		Ts:      timestamp,
	})
}

func (c *Client) RemoveReaction(channelID, timestamp, emoji string) error {
	ref := slackapi.ItemRef{
		Channel:   channelID,
		Timestamp: timestamp,
	}
	if err := c.api.RemoveReaction(emoji, ref); err != nil {
		return friendlyError(err)
	}
	return nil
}

// OpenConversation opens (or resumes) a DM or MPIM with the given user IDs.
// One user → DM, two-or-more → MPIM. The returned Channel is added to the
// cache so the sidebar can show it on the next publish.
func (c *Client) OpenConversation(userIDs []string) (*Channel, error) {
	if len(userIDs) == 0 {
		return nil, errors.New("no users specified")
	}
	ch, _, _, err := c.api.OpenConversation(&slackapi.OpenConversationParameters{
		Users:    userIDs,
		ReturnIM: len(userIDs) == 1,
	})
	if err != nil {
		return nil, fmt.Errorf("open conversation: %w", friendlyError(err))
	}

	out := &Channel{
		ID:        ch.ID,
		Name:      ch.Name,
		IsIM:      ch.IsIM,
		IsMPIM:    ch.IsMpIM,
		IsPrivate: ch.IsPrivate,
		Topic:     ch.Topic.Value,
		Purpose:   ch.Purpose.Value,
	}
	if ch.IsIM {
		out.UserID = ch.User
		if u := c.cache.GetUser(ch.User); u != nil {
			out.Name = imDisplayName(u, ch.User)
		} else if out.Name == "" {
			out.Name = ch.User
		}
	} else if ch.IsMpIM {
		// MPIM name from Slack looks like "mpdm-foo--bar--baz-1"; rebuild a
		// readable list of display names from the requested user IDs.
		names := make([]string, 0, len(userIDs))
		for _, uid := range userIDs {
			if uid == c.selfID {
				continue
			}
			if u, err := c.ResolveUser(uid); err == nil {
				names = append(names, imDisplayName(u, uid))
			} else {
				names = append(names, uid)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			out.Name = strings.Join(names, ", ")
		}
	}

	c.cache.SetChannels([]Channel{*out})
	if onUpdate := c.cache.OnUpdate; onUpdate != nil {
		onUpdate()
	}
	return out, nil
}

func (c *Client) MarkChannel(channelID, ts string) error {
	// Optimistically clear the unread count in cache. This eliminates the
	// necessity of managing the cache manually in the UI.
	c.cache.SetChannelUnread(channelID, 0, 0, ts, ts)

	if err := c.api.MarkConversation(channelID, ts); err != nil {
		return fmt.Errorf("mark channel: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) DownloadFile(url, destPath string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if strings.HasSuffix(req.URL.Host, ".slack.com") || req.URL.Host == "slack.com" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	client := newSlackHTTPClient(c.cookie)
	// Reuse redirect policy from ImageLoader logic if needed, but for direct
	// downloads a simpler policy might suffice. Let's use the one from images.go
	// to be safe as Slack 302s to CDNs.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if len(via) > 0 {
			if auth := via[0].Header.Get("Authorization"); auth != "" {
				if strings.HasSuffix(req.URL.Host, ".slack.com") || req.URL.Host == "slack.com" {
					req.Header.Set("Authorization", auth)
				}
			}
		}
		return nil
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func (c *Client) Search(query string) ([]SearchResult, error) {
	params := slackapi.SearchParameters{
		Count: 100,
	}
	msgs, err := c.api.SearchMessages(query, params)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	results := make([]SearchResult, 0, len(msgs.Matches))
	for _, match := range msgs.Matches {
		channelName := match.Channel.Name
		isIM := strings.HasPrefix(match.Channel.ID, "D")

		if ch := c.cache.GetChannel(match.Channel.ID); ch != nil && ch.Name != "" {
			channelName = ch.Name
		} else if isIM && strings.HasPrefix(channelName, "U") {
			if user, err := c.ResolveUser(channelName); err == nil {
				channelName = user.DisplayName
				if channelName == "" {
					channelName = user.Name
				}
			}
		}

		// The slack-go SearchMessage type doesn't expose thread_ts, but Slack
		// encodes it in the permalink as ?thread_ts=… for any match that lives
		// inside a thread. Recover it so callers can tell parent matches from
		// reply matches and walk back to the parent when needed.
		threadTS := ""
		if match.Permalink != "" {
			if u, err := url.Parse(match.Permalink); err == nil {
				threadTS = u.Query().Get("thread_ts")
			}
		}

		results = append(results, SearchResult{
			ChannelID:   match.Channel.ID,
			ChannelName: channelName,
			IsIM:        isIM,
			Message: Message{
				Timestamp: match.Timestamp,
				UserID:    match.User,
				Username:  match.Username,
				Text:      match.Text,
				ThreadTS:  threadTS,
			},
		})
	}
	return results, nil
}

const (
	SlackbotID = "USLACKBOT"
)

func (c *Client) RefreshUser(userID string) (*User, error) {
	if userID == "" {
		return nil, errors.New("empty user ID")
	}
	if strings.HasPrefix(userID, "B") {
		return c.ResolveBot(userID)
	}

	info, err := c.api.GetUserInfo(userID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	presence := "away"
	p, err := c.api.GetUserPresence(userID)
	if err == nil {
		presence = p.Presence
	}

	user := &User{
		ID:          info.ID,
		Name:        info.Name,
		RealName:    info.RealName,
		DisplayName: info.Profile.DisplayName,
		IsBot:       info.IsBot || info.ID == SlackbotID,
		Presence:    presence,
		StatusEmoji: info.Profile.StatusEmoji,
		StatusText:  info.Profile.StatusText,
		Title:       info.Profile.Title,
		Email:       info.Profile.Email,
		Phone:       info.Profile.Phone,
		Timezone:    info.TZ,
		ImageURL:    info.Profile.Image192,
	}
	if user.DisplayName == "" {
		user.DisplayName = info.RealName
	}
	if user.RealName == "" {
		user.RealName = user.DisplayName
	}

	if info.ID == SlackbotID && user.ImageURL == "" {
		if info.Profile.Image72 != "" {
			user.ImageURL = info.Profile.Image72
		} else if info.Profile.Image48 != "" {
			user.ImageURL = info.Profile.Image48
		}
	}

	if profile, err := c.api.GetUserProfile(&slackapi.GetUserProfileParameters{
		UserID: userID,
	}); err == nil {
		if profile.Email != "" {
			user.Email = profile.Email
		}
		if profile.Phone != "" {
			user.Phone = profile.Phone
		}
	}

	c.cache.SetUser(user)
	return user, nil
}

func (c *Client) ResolveUser(userID string) (*User, error) {
	if userID == "" {
		return nil, errors.New("empty user ID")
	}
	if strings.HasPrefix(userID, "B") {
		return c.ResolveBot(userID)
	}
	if user := c.cache.GetUser(userID); user != nil {
		return user, nil
	}
	return c.RefreshUser(userID)
}

func (c *Client) ResolveBot(botID string) (*User, error) {
	if user := c.cache.GetUser(botID); user != nil {
		return user, nil
	}

	bot, err := c.api.GetBotInfo(slackapi.GetBotInfoParameters{Bot: botID})
	if err != nil {
		return nil, fmt.Errorf("get bot: %w", err)
	}

	user := &User{
		ID:          bot.ID,
		Name:        bot.Name,
		DisplayName: bot.Name,
		RealName:    bot.Name,
		IsBot:       true,
		ImageURL:    bot.Icons.Image72,
	}
	if user.ImageURL == "" {
		user.ImageURL = bot.Icons.Image48
	}
	if user.ImageURL == "" {
		user.ImageURL = bot.Icons.Image36
	}

	c.cache.SetUser(user)
	return user, nil
}

func (c *Client) GetUserGroups() ([]UserGroup, error) {
	groups, err := c.api.GetUserGroups(slackapi.GetUserGroupsOptionIncludeDisabled(true))
	if err != nil {
		return nil, fmt.Errorf("get usergroups: %w", err)
	}

	result := make([]UserGroup, 0, len(groups))
	for _, g := range groups {
		result = append(result, UserGroup{
			ID:     g.ID,
			Handle: g.Handle,
			Name:   g.Name,
		})
	}
	c.cache.SetUserGroups(result)
	slog.Debug("loaded usergroups", "count", len(result))
	return result, nil
}

func (c *Client) convertMessage(msg slackapi.Message) Message {
	username := msg.Username
	userID := msg.User

	if msg.BotProfile != nil {
		if username == "" {
			username = msg.BotProfile.Name
		}
		if userID == "" {
			userID = msg.BotProfile.ID
		}
		if c.cache.GetUser(userID) == nil {
			c.cache.SetUser(&User{
				ID:          userID,
				Name:        msg.BotProfile.Name,
				DisplayName: msg.BotProfile.Name,
				RealName:    msg.BotProfile.Name,
				IsBot:       true,
				ImageURL:    msg.BotProfile.Icons.Image72,
			})
		}
	}

	if username == "" {
		if userID != "" {
			if user, err := c.ResolveUser(userID); err == nil {
				username = user.DisplayName
				if username == "" {
					username = user.Name
				}
			}
		} else if msg.BotID != "" {
			if bot, err := c.ResolveBot(msg.BotID); err == nil {
				username = bot.DisplayName
				userID = bot.ID
			}
		}
	}

	if username == "" {
		if userID != "" {
			username = userID
		} else if msg.BotID != "" {
			username = msg.BotID
			userID = msg.BotID
		}
	}

	reactions := make([]Reaction, 0, len(msg.Reactions))
	for _, r := range msg.Reactions {
		hasMe := false
		for _, u := range r.Users {
			if u == c.selfID {
				hasMe = true
				break
			}
		}
		reactions = append(reactions, Reaction{
			Name:  r.Name,
			Count: r.Count,
			Users: r.Users,
			HasMe: hasMe,
		})
	}

	files := make([]File, 0, len(msg.Files))
	for _, f := range msg.Files {
		ff := File{
			Name:     f.Name,
			URL:      f.URLPrivate,
			Mimetype: f.Mimetype,
		}
		// Pick a low-resolution thumbnail for the inline chat preview. The
		// preview is capped at ~320 dp; a 360px thumb is sharp enough on
		// 1x displays and still acceptable on HiDPI without dragging the
		// full original (often megabytes) over the wire on every scroll.
		switch {
		case f.Thumb360 != "":
			ff.Thumb, ff.ThumbW, ff.ThumbH = f.Thumb360, f.Thumb360W, f.Thumb360H
		case f.Thumb480 != "":
			ff.Thumb, ff.ThumbW, ff.ThumbH = f.Thumb480, f.Thumb480W, f.Thumb480H
		case f.Thumb720 != "":
			ff.Thumb, ff.ThumbW, ff.ThumbH = f.Thumb720, f.Thumb720W, f.Thumb720H
		}
		if ff.ThumbW == 0 && f.OriginalW > 0 {
			ff.ThumbW, ff.ThumbH = f.OriginalW, f.OriginalH
		}
		files = append(files, ff)
	}

	for _, b := range msg.Blocks.BlockSet {
		if ib, ok := b.(*slackapi.ImageBlock); ok {
			title := ""
			if ib.Title != nil {
				title = ib.Title.Text
			} else if ib.AltText != "" {
				title = ib.AltText
			} else {
				title = "image"
			}
			files = append(files, File{
				Name:     title,
				URL:      ib.ImageURL,
				Mimetype: detectMime(ib.ImageURL),
			})
		} else if sb, ok := b.(*slackapi.SectionBlock); ok && sb.Accessory != nil {
			if img := sb.Accessory.ImageElement; img != nil && img.ImageURL != nil {
				files = append(files, File{
					Name:     img.AltText,
					URL:      *img.ImageURL,
					Mimetype: detectMime(*img.ImageURL),
				})
			}
		} else if cb, ok := b.(*slackapi.ContextBlock); ok {
			for _, el := range cb.ContextElements.Elements {
				if img, ok := el.(*slackapi.ImageBlockElement); ok && img.ImageURL != nil {
					files = append(files, File{
						Name:     img.AltText,
						URL:      *img.ImageURL,
						Mimetype: detectMime(*img.ImageURL),
					})
				}
			}
		}
	}

	for _, a := range msg.Attachments {
		if c.disableLinkUnfurl && (a.FromURL != "" || a.OriginalURL != "") {
			continue
		}
		if a.ImageURL != "" {
			files = append(files, File{
				Name:     a.Title,
				URL:      a.ImageURL,
				Mimetype: detectMime(a.ImageURL),
			})
		} else if a.ThumbURL != "" {
			files = append(files, File{
				Name:     a.Title,
				URL:      a.ThumbURL,
				Mimetype: detectMime(a.ThumbURL),
			})
		}
	}

	text := msg.Text
	if text == "" {
		// Slack's modern composer omits the mrkdwn fallback for some message
		// shapes, leaving blocks as the only source of body content. Pull
		// from blocks so the message doesn't render as an empty row.
		text = c.extractBlockText(msg.Blocks.BlockSet)
	} else if blockText := c.extractBlockText(msg.Blocks.BlockSet); blockText != "" && !hasRichTextBlock(msg.Blocks.BlockSet) {
		// Bot messages frequently use SectionBlock/HeaderBlock/ContextBlock
		// with richer content than msg.Text — prefer that. RichTextBlock
		// content is just a re-encoding of msg.Text, so don't override.
		text = blockText
	}

	attText := c.extractAttachmentText(msg.Attachments)
	if attText != "" {
		if text != "" {
			text += "\n" + attText
		} else {
			text = attText
		}
	}

	editedTS := ""
	if msg.Edited != nil {
		editedTS = msg.Edited.Timestamp
	}

	if text == "" && len(files) == 0 {
		slog.Debug("unrendered message",
			"type", msg.Type,
			"subtype", msg.SubType,
			"bot_id", msg.BotID,
			"blocks", len(msg.Blocks.BlockSet),
			"attachments", len(msg.Attachments),
			"raw_text", msg.Text)
	}

	// Detect raw GIF URLs in text and treat them as images if no other files exist.
	if !c.disableMediaUnfurl && len(files) == 0 && text != "" {
		urls := ExtractURLs(text)
		if len(urls) == 1 {
			u := urls[0]
			low := strings.ToLower(u)
			isGif := strings.HasSuffix(low, ".gif") ||
				strings.Contains(low, ".gif?") ||
				strings.Contains(low, "giphy.com/media/") ||
				strings.Contains(low, "media.giphy.com/") ||
				strings.Contains(low, "tenor.com/view/") ||
				(strings.Contains(low, "giphy.com/gifs/") && !strings.Contains(low, "/html5"))

			if isGif {
				raw := strings.TrimSpace(text)
				slog.Debug("GIF detection candidate", "url", u, "text", raw)

				// Be lenient: check if the text is just the URL, or the URL wrapped in < >,
				// or if the text is exactly what Slack's ExtractURLs produced.
				cleanRaw := strings.Trim(raw, "<>")
				if cleanRaw == u || strings.HasPrefix(raw, "<"+u+"|") {
					files = append(files, File{
						Name:     "gif",
						URL:      u,
						Mimetype: "image/gif",
					})
					slog.Debug("GIF detected and added to files")
				} else {
					slog.Debug("GIF candidate rejected due to text mismatch", "text", raw, "u", u)
				}
			}
		} else if len(urls) > 0 {
			slog.Debug("Multiple URLs found, skipping GIF detection", "count", len(urls))
		}
	}

	return Message{
		Timestamp:   msg.Timestamp,
		UserID:      userID,
		Username:    username,
		Text:        text,
		ThreadTS:    msg.ThreadTimestamp,
		ReplyCount:  msg.ReplyCount,
		ReplyUsers:  msg.ReplyUsers,
		LastReplyTS: msg.LatestReply,
		Reactions:   reactions,
		Edited:      msg.Edited != nil,
		EditedTS:    editedTS,
		EditHistory: nil,
		Files:       files,
		IsBot:       msg.BotID != "" || msg.BotProfile != nil,
	}
}

func detectMime(url string) string {
	low := strings.ToLower(url)
	switch {
	case strings.HasSuffix(low, ".gif") || strings.Contains(low, ".gif?"):
		return "image/gif"
	case strings.Contains(low, "giphy.com/media/") || strings.Contains(low, "media.giphy.com/"):
		return "image/gif"
	case strings.Contains(low, "tenor.com/view/") || strings.Contains(low, "c.tenor.com/"):
		return "image/gif"
	case strings.HasSuffix(low, ".png") || strings.Contains(low, ".png?"):
		return "image/png"
	case strings.HasSuffix(low, ".webp") || strings.Contains(low, ".webp?"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func (c *Client) extractBlockText(blocks []slackapi.Block) string {
	var parts []string
	for _, b := range blocks {
		switch blk := b.(type) {
		case *slackapi.SectionBlock:
			if blk.Text != nil {
				parts = append(parts, blk.Text.Text)
			}
			for _, f := range blk.Fields {
				parts = append(parts, f.Text)
			}
		case *slackapi.ContextBlock:
			for _, el := range blk.ContextElements.Elements {
				if txt, ok := el.(*slackapi.TextBlockObject); ok {
					parts = append(parts, txt.Text)
				}
			}
		case *slackapi.HeaderBlock:
			if blk.Text != nil {
				parts = append(parts, blk.Text.Text)
			}
		case *slackapi.RichTextBlock:
			if t := richTextBlockToMarkdown(blk); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// richTextBlockToMarkdown reduces a Slack rich_text block to the legacy mrkdwn
// string form (mentions as <@U…>, links as <url|label>, emoji as :name:, etc.)
// so the existing Formatter pipeline can render it. Without this, messages
// composed in Slack's modern WYSIWYG editor that arrive with an empty msg.Text
// fallback render as blank rows.
func richTextBlockToMarkdown(blk *slackapi.RichTextBlock) string {
	var parts []string
	for _, el := range blk.Elements {
		switch e := el.(type) {
		case *slackapi.RichTextSection:
			parts = append(parts, strings.TrimSpace(richTextSectionToMarkdown(e.Elements)))
		case *slackapi.RichTextQuote:
			lines := strings.Split(strings.TrimSpace(richTextSectionToMarkdown(e.Elements)), "\n")
			for i, ln := range lines {
				lines[i] = "> " + ln
			}
			parts = append(parts, strings.Join(lines, "\n"))
		case *slackapi.RichTextPreformatted:
			parts = append(parts, "```\n"+strings.TrimSpace(richTextSectionToMarkdown(e.Elements))+"\n```")
		case *slackapi.RichTextList:
			for i, item := range e.Elements {
				if section, ok := item.(*slackapi.RichTextSection); ok {
					prefix := "• "
					if e.Style == slackapi.RTEListOrdered {
						prefix = fmt.Sprintf("%d. ", i+1)
					}
					parts = append(parts, prefix+strings.TrimSpace(richTextSectionToMarkdown(section.Elements)))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func richTextSectionToMarkdown(elements []slackapi.RichTextSectionElement) string {
	var b strings.Builder
	for _, el := range elements {
		switch e := el.(type) {
		case *slackapi.RichTextSectionTextElement:
			b.WriteString(applyStyle(e.Text, e.Style))
		case *slackapi.RichTextSectionUserElement:
			fmt.Fprintf(&b, "<@%s>", e.UserID)
		case *slackapi.RichTextSectionChannelElement:
			name := e.ChannelID
			if ch, ok := lookupChannelName(e.ChannelID); ok {
				name = ch
			}
			fmt.Fprintf(&b, "<#%s|%s>", e.ChannelID, name)
		case *slackapi.RichTextSectionEmojiElement:
			fmt.Fprintf(&b, ":%s:", e.Name)
		case *slackapi.RichTextSectionLinkElement:
			if e.Text != "" {
				fmt.Fprintf(&b, "<%s|%s>", e.URL, e.Text)
			} else {
				fmt.Fprintf(&b, "<%s>", e.URL)
			}
		case *slackapi.RichTextSectionBroadcastElement:
			fmt.Fprintf(&b, "<!%s>", e.Range)
		case *slackapi.RichTextSectionUserGroupElement:
			fmt.Fprintf(&b, "<!subteam^%s>", e.UsergroupID)
		case *slackapi.RichTextSectionTeamElement:
			fmt.Fprintf(&b, "<!team^%s>", e.TeamID)
		case *slackapi.RichTextSectionDateElement:
			b.WriteString(string(rune(e.Timestamp))) // best-effort; rarely seen
		}
	}
	return b.String()
}

func applyStyle(text string, style *slackapi.RichTextSectionTextStyle) string {
	if style == nil {
		return text
	}
	if style.Code {
		return "`" + text + "`"
	}
	if style.Bold {
		text = "*" + text + "*"
	}
	if style.Italic {
		text = "_" + text + "_"
	}
	if style.Strike {
		text = "~" + text + "~"
	}
	return text
}

func hasRichTextBlock(blocks []slackapi.Block) bool {
	for _, b := range blocks {
		if _, ok := b.(*slackapi.RichTextBlock); ok {
			return true
		}
	}
	return false
}

// channelNameLookup is set lazily by Client so the rich-text converter can
// resolve <#C…> ids to names without taking a Client receiver. It's optional —
// a nil resolver just produces the bare ID, which the formatter still renders
// safely.
var channelNameLookup func(string) (string, bool)

func lookupChannelName(id string) (string, bool) {
	if channelNameLookup == nil {
		return "", false
	}
	return channelNameLookup(id)
}

func (c *Client) extractAttachmentText(attachments []slackapi.Attachment) string {
	var parts []string
	for _, a := range attachments {
		if c.disableLinkUnfurl && (a.FromURL != "" || a.OriginalURL != "") {
			continue
		}
		var lines []string
		if a.Pretext != "" {
			lines = append(lines, a.Pretext)
		}
		if a.Title != "" {
			lines = append(lines, a.Title)
		}

		text := a.Text
		if text == "" {
			text = c.extractBlockText(a.Blocks.BlockSet)
		} else if blockText := c.extractBlockText(a.Blocks.BlockSet); blockText != "" && !hasRichTextBlock(a.Blocks.BlockSet) {
			text = blockText
		}

		if text != "" {
			lines = append(lines, text)
		}

		if len(lines) == 0 && a.Fallback != "" {
			lines = append(lines, a.Fallback)
		}

		if len(lines) > 0 {
			parts = append(parts, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(parts, "\n")
}
