package slack

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type Cache struct {
	mu         sync.RWMutex
	users      map[string]*User
	channels   map[string]*Channel
	messages   map[string][]Message
	threads    map[string][]Message
	usergroups map[string]*UserGroup

	store *sqliteStore

	// OnUpdate is called whenever the cache is significantly modified
	// (e.g. unread counts change).
	OnUpdate func()
}

// NewCache returns an in-memory cache backed by a local SQLite database for
// channels and message history. If SQLite can't be opened (e.g. read-only
// cache dir), the cache still works — it just doesn't persist between runs.
func NewCache() *Cache {
	c := &Cache{
		users:      make(map[string]*User),
		channels:   make(map[string]*Channel),
		messages:   make(map[string][]Message),
		threads:    make(map[string][]Message),
		usergroups: make(map[string]*UserGroup),
	}
	store, err := openSQLiteStore()
	if err != nil {
		slog.Warn("sqlite cache disabled", "error", err)
		return c
	}
	c.store = store
	if n, err := store.pruneOldMessages(); err != nil {
		slog.Warn("prune old messages failed", "error", err)
	} else if n > 0 {
		slog.Info("pruned old messages", "count", n)
	}
	return c
}

func cacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	dir := filepath.Join(d, "wlslack")
	os.MkdirAll(dir, 0755)
	return dir
}

// Close releases the underlying SQLite handle. Safe to call multiple times.
func (c *Cache) Close() error {
	if c == nil || c.store == nil {
		return nil
	}
	err := c.store.close()
	c.store = nil
	return err
}

// LoadChannelsFromDisk hydrates the in-memory channel map from SQLite.
// The name is kept for source compatibility with the old JSON-backed cache.
func (c *Cache) LoadChannelsFromDisk() ([]Channel, error) {
	if c.store == nil {
		return nil, nil
	}
	channels, err := c.store.loadAllChannels()
	if err != nil {
		return nil, err
	}
	if len(channels) > 0 {
		c.SetChannels(channels)
	}
	return channels, nil
}

func (c *Cache) SaveChannelsToDisk(channels []Channel) error {
	if c.store == nil {
		return nil
	}
	return c.store.saveChannels(channels)
}

func (c *Cache) LoadUsersFromDisk() ([]User, error) {
	if c.store == nil {
		return nil, nil
	}
	users, err := c.store.loadAllUsers()
	if err != nil {
		return nil, err
	}
	for i := range users {
		c.SetUser(&users[i])
	}
	return users, nil
}

func (c *Cache) SaveUsersToDisk(users []User) error {
	if c.store == nil {
		return nil
	}
	return c.store.saveUsers(users)
}

func (c *Cache) LoadUserGroupsFromDisk() ([]UserGroup, error) {
	if c.store == nil {
		return nil, nil
	}
	groups, err := c.store.loadAllUserGroups()
	if err != nil {
		return nil, err
	}
	c.SetUserGroups(groups)
	return groups, nil
}

func (c *Cache) SaveUserGroupsToDisk(groups []UserGroup) error {
	if c.store == nil {
		return nil
	}
	return c.store.saveUserGroups(groups)
}

// LoadMessagesFromDisk pulls a channel's recent (within retention) messages
// from SQLite into memory and returns them. Used to surface cached history
// instantly when the user opens a channel that hasn't been fetched this run.
func (c *Cache) LoadMessagesFromDisk(channelID string) ([]Message, error) {
	if c.store == nil {
		return nil, nil
	}
	since := time.Now().Add(-MessageRetention).Unix()
	msgs, err := c.store.loadMessages(channelID, "", since)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		c.SetMessages(channelID, msgs)
	}
	return msgs, nil
}

func (c *Cache) SaveMessagesToDisk(channelID string, messages []Message) error {
	if c.store == nil {
		return nil
	}
	return c.store.saveMessages(channelID, "", messages)
}

// SaveThreadMessagesToDisk persists thread replies under the parent thread's ts.
func (c *Cache) SaveThreadMessagesToDisk(channelID, threadTS string, messages []Message) error {
	if c.store == nil {
		return nil
	}
	return c.store.saveMessages(channelID, threadTS, messages)
}

// LoadThreadMessagesFromDisk returns thread replies cached within retention.
func (c *Cache) LoadThreadMessagesFromDisk(channelID, threadTS string) ([]Message, error) {
	if c.store == nil {
		return nil, nil
	}
	since := time.Now().Add(-MessageRetention).Unix()
	msgs, err := c.store.loadMessages(channelID, threadTS, since)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		c.SetThread(channelID, threadTS, msgs)
	}
	return msgs, nil
}

func (c *Cache) GetUser(id string) *User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.users[id]
}

func (c *Cache) SetUser(user *User) {
	c.mu.Lock()
	c.users[user.ID] = user
	c.mu.Unlock()

	if c.store != nil {
		go func() {
			if err := c.store.saveUsers([]User{*user}); err != nil {
				slog.Debug("persist user failed", "user", user.ID, "error", err)
			}
		}()
	}
}

func (c *Cache) SetChannels(channels []Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range channels {
		id := channels[i].ID
		newCh := channels[i]
		if existing, ok := c.channels[id]; ok {
			// Monotonicity for LatestTS: never move backwards.
			if existing.LatestTS > newCh.LatestTS {
				newCh.LatestTS = existing.LatestTS
			}

			// Monotonicity for LastReadTS: never move backwards. Slack's
			// eventually consistent responses often include stale read pointers.
			if existing.LastReadTS != "" && (newCh.LastReadTS == "" || newCh.LastReadTS < existing.LastReadTS) {
				newCh.LastReadTS = existing.LastReadTS
				newCh.UnreadCount = existing.UnreadCount
				newCh.MentionCount = existing.MentionCount
			}

			// MentionCount is set by the mention scan, not the channel
			// list payload. Preserve it across SetChannels writes
			// (GetChannels, ResolveConversationNames) so the sidebar
			// doesn't briefly drop the mention badge each refresh —
			// but only while the channel still has unreads. Once
			// unreads clear (e.g. read in another Slack client), the
			// mentions are gone too; preserving here would leave a
			// stale badge that points at an empty Mentions view.
			if newCh.MentionCount == 0 && existing.MentionCount > 0 && newCh.UnreadCount > 0 {
				newCh.MentionCount = existing.MentionCount
			}
		}
		if newCh.UnreadCount == 0 {
			newCh.MentionCount = 0
		}
		c.channels[id] = &newCh
	}
}

func (c *Cache) GetChannel(id string) *Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channels[id]
}

func (c *Cache) SetChannelUnread(id string, unreadCount, mentionCount int, lastReadTS, latestTS string) {
	c.mu.Lock()
	existing, ok := c.channels[id]
	if !ok {
		c.mu.Unlock()
		return
	}

	// Stale update protection: if the incoming lastReadTS is older than what we
	// already have, ignore the update. Slack's eventually consistent API often
	// returns stale unread counts/read pointers for several seconds after a
	// mark-as-read call.
	if lastReadTS != "" && existing.LastReadTS != "" && lastReadTS < existing.LastReadTS {
		c.mu.Unlock()
		return
	}

	updated := *existing
	updated.UnreadCount = unreadCount
	if unreadCount == 0 {
		mentionCount = 0
	}
	updated.MentionCount = mentionCount
	updated.LastReadTS = lastReadTS
	if latestTS != "" {
		updated.LatestTS = latestTS
	}
	c.channels[id] = &updated
	onUpdate := c.OnUpdate
	c.mu.Unlock()

	if onUpdate != nil {
		onUpdate()
	}

	if c.store != nil {
		if err := c.store.updateChannelUnread(id, unreadCount, mentionCount, lastReadTS, latestTS); err != nil {
			slog.Debug("persist unread failed", "channel", id, "error", err)
		}
	}
}

func (c *Cache) AdvanceChannelLatestTS(id, ts string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.channels[id]
	if !ok || ts <= existing.LatestTS {
		return false
	}
	updated := *existing
	updated.LatestTS = ts
	c.channels[id] = &updated
	return true
}

func (c *Cache) GetAllChannels() []Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	channels := make([]Channel, 0, len(c.channels))
	for _, ch := range c.channels {
		channels = append(channels, *ch)
	}
	sort.SliceStable(channels, func(i, j int) bool {
		if channels[i].LatestTS != channels[j].LatestTS {
			if channels[i].LatestTS == "" {
				return false
			}
			if channels[j].LatestTS == "" {
				return true
			}
			return channels[i].LatestTS > channels[j].LatestTS
		}
		if channels[i].Name != channels[j].Name {
			return channels[i].Name < channels[j].Name
		}
		return channels[i].ID < channels[j].ID
	})
	return channels
}

func (c *Cache) SetMessages(channelID string, msgs []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages[channelID] = msgs
}

// GetMessages returns the cached messages for a channel. If memory has none
// but SQLite has rows within retention, it loads them on the fly so the UI
// can render history instantly on first open after a restart.
func (c *Cache) GetMessages(channelID string) []Message {
	c.mu.RLock()
	msgs := c.messages[channelID]
	c.mu.RUnlock()
	if len(msgs) > 0 || c.store == nil {
		return msgs
	}
	loaded, err := c.LoadMessagesFromDisk(channelID)
	if err != nil {
		slog.Debug("load cached messages failed", "channel", channelID, "error", err)
		return nil
	}
	return loaded
}

func (c *Cache) SetThread(channelID, threadTS string, msgs []Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threads[channelID+":"+threadTS] = msgs
}

func (c *Cache) GetThread(channelID, threadTS string) []Message {
	c.mu.RLock()
	msgs := c.threads[channelID+":"+threadTS]
	c.mu.RUnlock()
	if len(msgs) > 0 || c.store == nil {
		return msgs
	}
	loaded, err := c.LoadThreadMessagesFromDisk(channelID, threadTS)
	if err != nil {
		slog.Debug("load cached thread failed", "channel", channelID, "thread", threadTS, "error", err)
		return nil
	}
	return loaded
}

func (c *Cache) SetUserGroups(groups []UserGroup) {
	c.mu.Lock()
	for i := range groups {
		c.usergroups[groups[i].ID] = &groups[i]
	}
	c.mu.Unlock()

	if c.store != nil {
		if err := c.store.saveUserGroups(groups); err != nil {
			slog.Debug("persist usergroups failed", "error", err)
		}
	}
}

func (c *Cache) GetAllUsers() []User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]User, 0, len(c.users))
	for _, u := range c.users {
		out = append(out, *u)
	}
	return out
}

func (c *Cache) GetAllUserGroups() []UserGroup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]UserGroup, 0, len(c.usergroups))
	for _, g := range c.usergroups {
		out = append(out, *g)
	}
	return out
}

func (c *Cache) GetUserGroup(id string) *UserGroup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usergroups[id]
}
