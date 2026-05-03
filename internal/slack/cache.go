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
	defer c.mu.Unlock()
	c.users[user.ID] = user
}

func (c *Cache) SetChannels(channels []Channel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range channels {
		id := channels[i].ID
		newCh := channels[i]
		if existing, ok := c.channels[id]; ok {
			if existing.LatestTS > newCh.LatestTS {
				newCh.LatestTS = existing.LatestTS
			}
			if newCh.LastReadTS == "" && existing.LastReadTS != "" {
				newCh.UnreadCount = existing.UnreadCount
				newCh.LastReadTS = existing.LastReadTS
			}
		}
		c.channels[id] = &newCh
	}
}

func (c *Cache) GetChannel(id string) *Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.channels[id]
}

func (c *Cache) SetChannelUnread(id string, unreadCount int, lastReadTS, latestTS string) {
	c.mu.Lock()
	existing, ok := c.channels[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	updated := *existing
	updated.UnreadCount = unreadCount
	updated.LastReadTS = lastReadTS
	if latestTS != "" {
		updated.LatestTS = latestTS
	}
	c.channels[id] = &updated
	c.mu.Unlock()

	if c.store != nil {
		if err := c.store.updateChannelUnread(id, unreadCount, lastReadTS, latestTS); err != nil {
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
		return channels[i].Name < channels[j].Name
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
	defer c.mu.Unlock()
	for i := range groups {
		c.usergroups[groups[i].ID] = &groups[i]
	}
}

func (c *Cache) GetUserGroup(id string) *UserGroup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usergroups[id]
}
