package slack

type Channel struct {
	ID          string
	Name        string
	UserID      string // for IMs, the other user's Slack ID
	IsIM        bool
	IsMPIM      bool
	IsPrivate   bool
	IsExternal  bool // ext-shared / org-shared with another workspace
	Topic       string
	Purpose     string
	UnreadCount  int
	MentionCount int
	LastReadTS   string
	LatestTS    string
	LatestTSVerified bool
}

type Message struct {
	Timestamp   string
	ChannelID   string // for consolidated views
	ChannelName string // for consolidated views
	UserID      string
	Username    string
	Text        string
	ThreadTS    string
	ReplyCount  int
	ReplyUsers  []string // IDs of users who replied
	LastReplyTS string   // Timestamp of the last reply
	Reactions   []Reaction
	Edited      bool
	EditedTS    string
	EditHistory []Edit
	Deleted     bool
	Files       []File
	IsBot       bool
}

type Edit struct {
	Timestamp string
	Text      string
}

type Reaction struct {
	Name  string
	Count int
	Users []string
	HasMe bool
}

type User struct {
	ID          string
	Name        string
	RealName    string
	DisplayName string
	IsBot       bool
	Presence    string
	StatusEmoji string
	StatusText  string
	Title       string
	Email       string
	Phone       string
	Timezone    string
	ImageURL    string
}

type File struct {
	Name     string
	URL      string // url_private — the full-resolution original
	Thumb    string // a pre-resized thumbnail when the file is an image
	ThumbW   int
	ThumbH   int
	Mimetype string
}

// IsImage reports whether the file should render as an inline image.
func (f File) IsImage() bool {
	return len(f.Mimetype) >= 6 && f.Mimetype[:6] == "image/"
}

// PreferredImageURL returns the best URL to fetch for full-resolution
// rendering (e.g. the in-app image viewer). Slack's `files-tmb/...` thumbnail
// URLs reject Bearer auth from xoxp tokens in some workspaces (the request
// gets 302'd to the workspace login page), while `url_private`
// (`files-pri/...`) accepts the same Authorization header reliably — that's
// the URL slack-go's own GetFile helper uses.
func (f File) PreferredImageURL() string {
	if f.URL != "" {
		return f.URL
	}
	return f.Thumb
}

// ThumbnailURL returns the URL to fetch for inline preview rendering in the
// chat history. Prefers the pre-resized Slack thumbnail so we don't pull a
// multi-megabyte original just to draw a 320 dp box; falls back to the
// full-resolution URL when no thumbnail is available.
func (f File) ThumbnailURL() string {
	if f.Thumb != "" {
		return f.Thumb
	}
	return f.URL
}

type UserGroup struct {
	ID     string
	Handle string
	Name   string
}

type Thread struct {
	ID            string
	ChannelID     string
	ChannelName   string
	ThreadTS      string
	Message       Message
	LastReplyTS   string
	UnreadReplies int
}

func (m *Message) AllURLs() []string {
	urls := ExtractURLs(m.Text)
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		seen[u] = true
	}
	for _, f := range m.Files {
		if f.URL != "" && !seen[f.URL] {
			urls = append(urls, f.URL)
			seen[f.URL] = true
		}
	}
	return urls
}

type SearchResult struct {
	ChannelID   string
	ChannelName string
	IsIM        bool
	Message     Message
}
