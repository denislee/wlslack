package slack

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

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

func (c *Client) MergeMessages(a, b []Message) []Message {
	m := make(map[string]Message, len(a)+len(b))
	for _, msg := range a {
		m[msg.Timestamp] = msg
	}
	for _, msg := range b {
		existing, ok := m[msg.Timestamp]
		if ok {
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
		}
		m[msg.Timestamp] = msg
	}

	merged := make([]Message, 0, len(m))
	for _, msg := range m {
		merged = append(merged, msg)
	}

	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp < merged[j].Timestamp
	})

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
		api:   api,
		cache: NewCache(),
	}
	channelNameLookup = func(id string) (string, bool) {
		if ch := c.cache.GetChannel(id); ch != nil && ch.Name != "" {
			return ch.Name, true
		}
		return "", false
	}
	return c, nil
}

func (c *Client) AuthTest() (string, error) {
	resp, err := c.api.AuthTest()
	if err != nil {
		return "", fmt.Errorf("auth test failed: %w", err)
	}
	return resp.UserID, nil
}

// VerifyFileAccess pokes files.list with a tiny page size so we can tell
// "token lacks files:read scope" apart from "files.slack.com is rejecting the
// CDN auth handshake" — both look identical in the image-loader logs.
// Returns a typed error so callers can format actionable advice.
func (c *Client) VerifyFileAccess() error {
	_, _, err := c.api.ListFiles(slackapi.ListFilesParameters{Limit: 1})
	if err == nil {
		return nil
	}
	var serr slackapi.SlackErrorResponse
	if errors.As(err, &serr) && serr.Err == "missing_scope" {
		return fmt.Errorf("token lacks files:read scope: regenerate the token with files:read (and files:write if you want uploads)")
	}
	if strings.Contains(err.Error(), "missing_scope") {
		return fmt.Errorf("token lacks files:read scope: regenerate the token with files:read (and files:write if you want uploads)")
	}
	return fmt.Errorf("files.list probe failed: %w", err)
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
			if ch.Latest != nil {
				cc.LatestTS = ch.Latest.Timestamp
			}
			allChannels = append(allChannels, cc)
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	slog.Info("GetChannels list fetched", "total", len(allChannels), "pages", page)

	c.enrichWithUnreadCounts(allChannels, priorityIDs)

	unreadCount := 0
	for _, ch := range allChannels {
		if ch.UnreadCount > 0 {
			unreadCount++
		}
	}
	slog.Info("GetChannels done", "total", len(allChannels), "with_unread", unreadCount)

	c.cache.SetChannels(allChannels)
	_ = c.cache.SaveChannelsToDisk(allChannels)
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

// ResolveIMNames fills in display names for IM channels in parallel, using the
// per-user resolution cache. It mutates channels in place, then refreshes the
// in-memory cache and on-disk snapshot. Safe to call from a goroutine after
// GetChannels has returned.
func (c *Client) ResolveIMNames(channels []Channel) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)

	for i := range channels {
		if !channels[i].IsIM || channels[i].UserID == "" {
			continue
		}
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
				slog.Debug("ResolveIMNames: resolve failed", "user", channels[idx].UserID, "error", err)
				return
			}
			channels[idx].Name = imDisplayName(user, channels[idx].UserID)
		}(i)
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
		c.cache.SetChannelUnread(ch.ID, ch.UnreadCount, ch.LastReadTS, ch.LatestTS)
	}

	return channels, nil
}

func (c *Client) enrichWithUnreadCounts(channels []Channel, priorityIDs []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	priorityMap := make(map[string]bool)
	for _, id := range priorityIDs {
		priorityMap[id] = true
	}

	for i := range channels {
		isPriority := priorityMap[channels[i].ID] || channels[i].UnreadCount > 0
		if len(channels) > 50 && !isPriority {
			continue
		}

		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info, err := c.api.GetConversationInfo(&slackapi.GetConversationInfoInput{
				ChannelID: channels[idx].ID,
			})
			if err != nil {
				slog.Debug("conversations.info error", "channel", channels[idx].Name, "error", err)
				return
			}
			channels[idx].UnreadCount = info.UnreadCountDisplay
			channels[idx].LastReadTS = info.LastRead
			if info.Latest != nil {
				channels[idx].LatestTS = info.Latest.Timestamp
			}
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

	c.cache.SetMessages(channelID, messages)
	_ = c.cache.SaveMessagesToDisk(channelID, messages)
	return messages, nil
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

	c.cache.SetThread(channelID, threadTS, replies)
	_ = c.cache.SaveThreadMessagesToDisk(channelID, threadTS, replies)
	return replies, nil
}

func (c *Client) SendMessage(channelID, text string) error {
	_, _, err := c.api.PostMessage(
		channelID,
		slackapi.MsgOptionText(text, false),
	)
	if err != nil {
		return fmt.Errorf("send message: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) UpdateMessage(channelID, timestamp, text string) error {
	_, _, _, err := c.api.UpdateMessage(
		channelID,
		timestamp,
		slackapi.MsgOptionText(text, false),
	)
	if err != nil {
		return fmt.Errorf("update message: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) SendThreadReply(channelID, threadTS, text string) error {
	_, _, err := c.api.PostMessage(
		channelID,
		slackapi.MsgOptionText(text, false),
		slackapi.MsgOptionTS(threadTS),
	)
	if err != nil {
		return fmt.Errorf("send reply: %w", friendlyError(err))
	}
	return nil
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

func (c *Client) MarkChannel(channelID, ts string) error {
	if err := c.api.MarkConversation(channelID, ts); err != nil {
		return fmt.Errorf("mark channel: %w", friendlyError(err))
	}
	return nil
}

func (c *Client) Search(query string) ([]SearchResult, error) {
	params := slackapi.SearchParameters{
		Sort:          "timestamp",
		SortDirection: "desc",
		Count:         20,
	}
	msgs, err := c.api.SearchMessages(query, params)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	results := make([]SearchResult, 0, len(msgs.Matches))
	for _, match := range msgs.Matches {
		channelName := match.Channel.Name
		isIM := strings.HasPrefix(match.Channel.ID, "D")
		if isIM && strings.HasPrefix(channelName, "U") {
			if user, err := c.ResolveUser(channelName); err == nil {
				channelName = user.DisplayName
				if channelName == "" {
					channelName = user.Name
				}
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
			},
		})
	}
	return results, nil
}

func (c *Client) ResolveUser(userID string) (*User, error) {
	if user := c.cache.GetUser(userID); user != nil {
		return user, nil
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
		IsBot:       info.IsBot,
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

func (c *Client) GetUserGroups() ([]UserGroup, error) {
	groups, err := c.api.GetUserGroups()
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
	return result, nil
}

func (c *Client) convertMessage(msg slackapi.Message) Message {
	username := msg.Username
	if username == "" {
		if user, err := c.ResolveUser(msg.User); err == nil {
			username = user.DisplayName
			if username == "" {
				username = user.Name
			}
		} else {
			username = msg.User
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
				Mimetype: "image/jpeg", // fallback so IsImage() works
			})
		}
	}

	for _, a := range msg.Attachments {
		if a.ImageURL != "" {
			files = append(files, File{
				Name:     a.Title,
				URL:      a.ImageURL,
				Mimetype: "image/jpeg",
			})
		} else if a.ThumbURL != "" {
			files = append(files, File{
				Name:     a.Title,
				URL:      a.ThumbURL,
				Mimetype: "image/jpeg",
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
			text += "\n\n" + attText
		} else {
			text = attText
		}
	}

	editedTS := ""
	if msg.Edited != nil {
		editedTS = msg.Edited.Timestamp
	}

	return Message{
		Timestamp:   msg.Timestamp,
		UserID:      msg.User,
		Username:    username,
		Text:        text,
		ThreadTS:    msg.ThreadTimestamp,
		ReplyCount:  msg.ReplyCount,
		Reactions:   reactions,
		Edited:      msg.Edited != nil,
		EditedTS:    editedTS,
		EditHistory: nil,
		Files:       files,
		IsBot:       msg.BotID != "",
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
			parts = append(parts, richTextSectionToMarkdown(e.Elements))
		case *slackapi.RichTextQuote:
			lines := strings.Split(richTextSectionToMarkdown(e.Elements), "\n")
			for i, ln := range lines {
				lines[i] = "> " + ln
			}
			parts = append(parts, strings.Join(lines, "\n"))
		case *slackapi.RichTextPreformatted:
			parts = append(parts, "```\n"+richTextSectionToMarkdown(e.Elements)+"\n```")
		case *slackapi.RichTextList:
			for i, item := range e.Elements {
				if section, ok := item.(*slackapi.RichTextSection); ok {
					prefix := "• "
					if e.Style == slackapi.RTEListOrdered {
						prefix = fmt.Sprintf("%d. ", i+1)
					}
					parts = append(parts, prefix+richTextSectionToMarkdown(section.Elements))
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
		var lines []string
		if a.Pretext != "" {
			lines = append(lines, a.Pretext)
		}
		if a.Title != "" {
			lines = append(lines, a.Title)
		}
		if a.Text != "" {
			lines = append(lines, a.Text)
		}
		if len(lines) > 0 {
			parts = append(parts, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(parts, "\n\n")
}
