package repository

import (
	"context"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/internal/wechatdb/datasource"
	"github.com/sjzar/chatlog/internal/wechatdb/indexer"
)

// Repository 实现了 repository.Repository 接口
type Repository struct {
	ds datasource.DataSource

	indexPath        string
	index            *indexer.Index
	indexMu          sync.Mutex
	indexStatus      model.SearchIndexStatus
	indexFingerprint string
	indexCtx         context.Context
	indexCancel      context.CancelFunc

	// Cache for contact
	contactCache      map[string]*model.Contact
	aliasToContact    map[string][]*model.Contact
	remarkToContact   map[string][]*model.Contact
	nickNameToContact map[string][]*model.Contact
	chatRoomInContact map[string]*model.Contact
	contactList       []string
	aliasList         []string
	remarkList        []string
	nickNameList      []string

	// Cache for chat room
	chatRoomCache      map[string]*model.ChatRoom
	remarkToChatRoom   map[string][]*model.ChatRoom
	nickNameToChatRoom map[string][]*model.ChatRoom
	chatRoomList       []string
	chatRoomRemark     []string
	chatRoomNickName   []string

	// 快速查找索引
	chatRoomUserToInfo map[string]*model.Contact
}

// New 创建一个新的 Repository
func New(ds datasource.DataSource, indexPath string) (*Repository, error) {
	r := &Repository{
		ds:                 ds,
		indexPath:          indexPath,
		contactCache:       make(map[string]*model.Contact),
		aliasToContact:     make(map[string][]*model.Contact),
		remarkToContact:    make(map[string][]*model.Contact),
		nickNameToContact:  make(map[string][]*model.Contact),
		chatRoomUserToInfo: make(map[string]*model.Contact),
		contactList:        make([]string, 0),
		aliasList:          make([]string, 0),
		remarkList:         make([]string, 0),
		nickNameList:       make([]string, 0),
		chatRoomCache:      make(map[string]*model.ChatRoom),
		remarkToChatRoom:   make(map[string][]*model.ChatRoom),
		nickNameToChatRoom: make(map[string][]*model.ChatRoom),
		chatRoomList:       make([]string, 0),
		chatRoomRemark:     make([]string, 0),
		chatRoomNickName:   make([]string, 0),
	}

	// 初始化缓存
	if err := r.initCache(context.Background()); err != nil {
		return nil, errors.InitCacheFailed(err)
	}

	ds.SetCallback("contact", r.contactCallback)
	ds.SetCallback("chatroom", r.chatroomCallback)

	if err := r.initIndex(); err != nil {
		log.Warn().Err(err).Msg("init fts index failed")
	}

	return r, nil
}

// initCache 初始化缓存
func (r *Repository) initCache(ctx context.Context) error {
	// 初始化联系人缓存
	if err := r.initContactCache(ctx); err != nil {
		return err
	}

	// 初始化群聊缓存
	if err := r.initChatRoomCache(ctx); err != nil {
		return err
	}

	return nil
}

func (r *Repository) contactCallback(event fsnotify.Event) error {
	if !(event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Rename) || event.Op.Has(fsnotify.Remove)) {
		return nil
	}
	if err := r.initContactCache(context.Background()); err != nil {
		log.Err(err).Msgf("Failed to reinitialize contact cache: %s", event.Name)
	}
	return nil
}

func (r *Repository) chatroomCallback(event fsnotify.Event) error {
	if !(event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Rename) || event.Op.Has(fsnotify.Remove)) {
		return nil
	}
	if err := r.initChatRoomCache(context.Background()); err != nil {
		log.Err(err).Msgf("Failed to reinitialize contact cache: %s", event.Name)
	}
	return nil
}

// Close 实现 Repository 接口的 Close 方法
func (r *Repository) Close() error {
	if r.indexCancel != nil {
		r.indexCancel()
		r.indexCancel = nil
	}

	var firstErr error
	if r.index != nil {
		if err := r.index.Close(); err != nil {
			firstErr = err
		}
		r.index = nil
	}

	if err := r.ds.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// GetAvatar proxies to datasource
func (r *Repository) GetAvatar(ctx context.Context, username string, size string) (*model.Avatar, error) {
	return r.ds.GetAvatar(ctx, username, size)
}

// Stats proxies
func (r *Repository) GlobalMessageStats(ctx context.Context) (*model.GlobalMessageStats, error) {
	return r.ds.GlobalMessageStats(ctx)
}
func (r *Repository) GroupMessageCounts(ctx context.Context) (map[string]int64, error) {
	return r.ds.GroupMessageCounts(ctx)
}
func (r *Repository) MonthlyTrend(ctx context.Context, months int) ([]model.MonthlyTrend, error) {
	return r.ds.MonthlyTrend(ctx, months)
}
func (r *Repository) Heatmap(ctx context.Context) ([24][7]int64, error) {
	return r.ds.Heatmap(ctx)
}

func (r *Repository) GlobalTodayHourly(ctx context.Context) ([24]int64, error) {
	if ds, ok := r.ds.(interface {
		GlobalTodayHourly(context.Context) ([24]int64, error)
	}); ok {
		return ds.GlobalTodayHourly(ctx)
	}
	return [24]int64{}, nil
}

// IntimacyBase proxies
func (r *Repository) IntimacyBase(ctx context.Context) (map[string]*model.IntimacyBase, error) {
	return r.ds.IntimacyBase(ctx)
}
func (r *Repository) GroupTodayMessageCounts(ctx context.Context) (map[string]int64, error) {
	if ds, ok := r.ds.(interface {
		GroupTodayMessageCounts(context.Context) (map[string]int64, error)
	}); ok {
		return ds.GroupTodayMessageCounts(ctx)
	}
	return map[string]int64{}, nil
}

func (r *Repository) GroupTodayHourly(ctx context.Context) (map[string][24]int64, error) {
	if ds, ok := r.ds.(interface {
		GroupTodayHourly(context.Context) (map[string][24]int64, error)
	}); ok {
		return ds.GroupTodayHourly(ctx)
	}
	return map[string][24]int64{}, nil
}

func (r *Repository) GroupWeekMessageCount(ctx context.Context) (int64, error) {
	if ds, ok := r.ds.(interface {
		GroupWeekMessageCount(context.Context) (int64, error)
	}); ok {
		return ds.GroupWeekMessageCount(ctx)
	}
	return 0, nil
}

func (r *Repository) GroupMessageTypeStats(ctx context.Context) (map[string]int64, error) {
	if ds, ok := r.ds.(interface {
		GroupMessageTypeStats(context.Context) (map[string]int64, error)
	}); ok {
		return ds.GroupMessageTypeStats(ctx)
	}
	return map[string]int64{}, nil
}
