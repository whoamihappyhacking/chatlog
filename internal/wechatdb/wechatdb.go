package wechatdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"

	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/internal/wechatdb/datasource"
	"github.com/sjzar/chatlog/internal/wechatdb/repository"
)

type DB struct {
	path     string
	platform string
	version  int
	ds       datasource.DataSource
	repo     *repository.Repository
}

func New(path string, platform string, version int) (*DB, error) {

	w := &DB{
		path:     path,
		platform: platform,
		version:  version,
	}

	// 初始化，加载数据库文件信息
	if err := w.Initialize(); err != nil {
		return nil, err
	}

	return w, nil
}

func (w *DB) Close() error {
	if w.repo != nil {
		return w.repo.Close()
	}
	return nil
}

func (w *DB) Initialize() error {
	var err error
	w.ds, err = datasource.New(w.path, w.platform, w.version)
	if err != nil {
		return err
	}

	indexPath := filepath.Join(w.path, "indexes", "messages")
	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		return fmt.Errorf("prepare index directory: %w", err)
	}
	w.repo, err = repository.New(w.ds, indexPath)
	if err != nil {
		return err
	}

	return nil
}

func (w *DB) GetMessages(start, end time.Time, talker string, sender string, keyword string, limit, offset int) ([]*model.Message, error) {
	ctx := context.Background()

	// 使用 repository 获取消息
	messages, err := w.repo.GetMessages(ctx, start, end, talker, sender, keyword, limit, offset)
	if err != nil {
		return nil, err
	}

	return messages, nil
}

func (w *DB) SearchMessages(req *model.SearchRequest) (*model.SearchResponse, error) {
	ctx := context.Background()
	return w.repo.SearchMessages(ctx, req)
}

func (w *DB) IndexMessages(messages []*model.Message) error {
	if w.repo == nil {
		return fmt.Errorf("repository not initialized")
	}
	return w.repo.IndexMessages(context.Background(), messages)
}

type GetContactsResp struct {
	Items []*model.Contact `json:"items"`
}

func (w *DB) GetContacts(key string, limit, offset int) (*GetContactsResp, error) {
	ctx := context.Background()

	contacts, err := w.repo.GetContacts(ctx, key, limit, offset)
	if err != nil {
		return nil, err
	}

	return &GetContactsResp{
		Items: contacts,
	}, nil
}

type GetChatRoomsResp struct {
	Items []*model.ChatRoom `json:"items"`
}

func (w *DB) GetChatRooms(key string, limit, offset int) (*GetChatRoomsResp, error) {
	ctx := context.Background()

	chatRooms, err := w.repo.GetChatRooms(ctx, key, limit, offset)
	if err != nil {
		return nil, err
	}

	return &GetChatRoomsResp{
		Items: chatRooms,
	}, nil
}

type GetSessionsResp struct {
	Items []*model.Session `json:"items"`
}

func (w *DB) GetSessions(key string, limit, offset int) (*GetSessionsResp, error) {
	ctx := context.Background()

	// 使用 repository 获取会话列表
	sessions, err := w.repo.GetSessions(ctx, key, limit, offset)
	if err != nil {
		return nil, err
	}

	return &GetSessionsResp{
		Items: sessions,
	}, nil
}

func (w *DB) GetMedia(_type string, key string) (*model.Media, error) {
	return w.repo.GetMedia(context.Background(), _type, key)
}

func (w *DB) SetCallback(group string, callback func(event fsnotify.Event) error) error {
	return w.ds.SetCallback(group, callback)
}

func (w *DB) GetAvatar(username string, size string) (*model.Avatar, error) {
	return w.repo.GetAvatar(context.Background(), username, size)
}

// Stats exposure
func (w *DB) GlobalMessageStats() (*model.GlobalMessageStats, error) {
	return w.repo.GlobalMessageStats(context.Background())
}
func (w *DB) GroupMessageCounts() (map[string]int64, error) {
	return w.repo.GroupMessageCounts(context.Background())
}
func (w *DB) MonthlyTrend(months int) ([]model.MonthlyTrend, error) {
	return w.repo.MonthlyTrend(context.Background(), months)
}
func (w *DB) Heatmap() ([24][7]int64, error) {
	return w.repo.Heatmap(context.Background())
}

func (w *DB) GlobalTodayHourly() ([24]int64, error) {
	return w.repo.GlobalTodayHourly(context.Background())
}

func (w *DB) IntimacyBase() (map[string]*model.IntimacyBase, error) {
	return w.repo.IntimacyBase(context.Background())
}

func (w *DB) GroupTodayMessageCounts() (map[string]int64, error) {
	return w.repo.GroupTodayMessageCounts(context.Background())
}

func (w *DB) GroupTodayHourly() (map[string][24]int64, error) {
	return w.repo.GroupTodayHourly(context.Background())
}

func (w *DB) GroupWeekMessageCount() (int64, error) {
	return w.repo.GroupWeekMessageCount(context.Background())
}

func (w *DB) GroupMessageTypeStats() (map[string]int64, error) {
	return w.repo.GroupMessageTypeStats(context.Background())
}
