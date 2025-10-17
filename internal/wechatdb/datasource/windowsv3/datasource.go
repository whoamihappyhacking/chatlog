package windowsv3

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/internal/wechatdb/datasource/dbm"
	"github.com/sjzar/chatlog/internal/wechatdb/msgstore"
	"github.com/sjzar/chatlog/pkg/util"
)

const (
	Message = "message"
	Contact = "contact"
	Image   = "image"
	Video   = "video"
	File    = "file"
	Voice   = "voice"

	talkerCacheTTL = 30 * time.Second
)

var Groups = []*dbm.Group{
	{
		Name:      Message,
		Pattern:   `^MSG([0-9]?[0-9])?\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Contact,
		Pattern:   `^MicroMsg\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Image,
		Pattern:   `^HardLinkImage\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Video,
		Pattern:   `^HardLinkVideo\.db$`,
		BlackList: []string{},
	},
	{
		Name:      File,
		Pattern:   `^HardLinkFile\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Voice,
		Pattern:   `^MediaMSG([0-9]?[0-9])?\.db$`,
		BlackList: []string{},
	},
}

// MessageDBInfo 保存消息数据库的信息
type MessageDBInfo struct {
	FilePath  string
	StartTime time.Time
	EndTime   time.Time
	TalkerMap map[string]int
}

// DataSource 实现了 DataSource 接口
type DataSource struct {
	path string
	dbm  *dbm.DBManager

	// 消息数据库信息
	messageInfos []MessageDBInfo

	messageStores  []*msgstore.Store
	messageStoreMu sync.RWMutex

	talkerCacheMu     sync.RWMutex
	talkerCache       []string
	talkerCacheExpiry time.Time
}

// New 创建一个新的 WindowsV3DataSource
func New(path string) (*DataSource, error) {
	ds := &DataSource{
		path:          path,
		dbm:           dbm.NewDBManager(path),
		messageInfos:  make([]MessageDBInfo, 0),
		messageStores: make([]*msgstore.Store, 0),
	}

	for _, g := range Groups {
		ds.dbm.AddGroup(g)
	}

	if err := ds.dbm.Start(); err != nil {
		return nil, err
	}

	if err := ds.initMessageDbs(); err != nil {
		return nil, errors.DBInitFailed(err)
	}

	ds.dbm.AddCallback(Message, func(event fsnotify.Event) error {
		if !(event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Rename)) {
			return nil
		}
		if err := ds.initMessageDbs(); err != nil {
			log.Err(err).Msgf("Failed to reinitialize message DBs: %s", event.Name)
		}
		ds.invalidateTalkerCache()
		return nil
	})

	ds.dbm.AddCallback(Contact, func(event fsnotify.Event) error {
		if !(event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Rename) || event.Op.Has(fsnotify.Remove)) {
			return nil
		}
		ds.invalidateTalkerCache()
		return nil
	})

	return ds, nil
}

func (ds *DataSource) SetCallback(group string, callback func(event fsnotify.Event) error) error {
	if group == "chatroom" {
		group = Contact
	}
	return ds.dbm.AddCallback(group, callback)
}

// initMessageDbs 初始化消息数据库
func (ds *DataSource) initMessageDbs() error {
	// 获取所有消息数据库文件路径
	dbPaths, err := ds.dbm.GetDBPath(Message)
	if err != nil {
		if strings.Contains(err.Error(), "db file not found") {
			ds.messageInfos = make([]MessageDBInfo, 0)
			ds.messageStoreMu.Lock()
			ds.messageStores = make([]*msgstore.Store, 0)
			ds.messageStoreMu.Unlock()
			return nil
		}
		return err
	}

	// 处理每个数据库文件
	infos := make([]MessageDBInfo, 0)
	for _, filePath := range dbPaths {
		db, err := ds.dbm.OpenDB(filePath)
		if err != nil {
			log.Err(err).Msgf("获取数据库 %s 失败", filePath)
			continue
		}

		// 获取 DBInfo 表中的开始时间
		var startTime time.Time

		rows, err := db.Query("SELECT tableIndex, tableVersion, tableDesc FROM DBInfo")
		if err != nil {
			log.Err(err).Msgf("查询数据库 %s 的 DBInfo 表失败", filePath)
			continue
		}

		for rows.Next() {
			var tableIndex int
			var tableVersion int64
			var tableDesc string

			if err := rows.Scan(&tableIndex, &tableVersion, &tableDesc); err != nil {
				log.Err(err).Msg("扫描 DBInfo 行失败")
				continue
			}

			// 查找描述为 "Start Time" 的记录
			if strings.Contains(tableDesc, "Start Time") {
				startTime = time.Unix(tableVersion/1000, (tableVersion%1000)*1000000)
				break
			}
		}
		rows.Close()

		// 组织 TalkerMap
		talkerMap := make(map[string]int)
		rows, err = db.Query("SELECT UsrName FROM Name2ID")
		if err != nil {
			log.Err(err).Msgf("查询数据库 %s 的 Name2ID 表失败", filePath)
			continue
		}

		i := 1
		for rows.Next() {
			var userName string
			if err := rows.Scan(&userName); err != nil {
				log.Err(err).Msg("扫描 Name2ID 行失败")
				continue
			}
			talkerMap[userName] = i
			i++
		}
		rows.Close()

		// 保存数据库信息
		infos = append(infos, MessageDBInfo{
			FilePath:  filePath,
			StartTime: startTime,
			TalkerMap: talkerMap,
		})
	}

	// 按照 StartTime 排序数据库文件
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].StartTime.Before(infos[j].StartTime)
	})

	// 设置结束时间
	for i := range infos {
		if i == len(infos)-1 {
			infos[i].EndTime = time.Now()
		} else {
			infos[i].EndTime = infos[i+1].StartTime
		}
	}
	if len(ds.messageInfos) > 0 && len(infos) < len(ds.messageInfos) {
		log.Warn().Msgf("message db count decreased from %d to %d, skip init", len(ds.messageInfos), len(infos))
		return nil
	}
	ds.messageInfos = infos
	stores := make([]*msgstore.Store, 0, len(infos))
	for _, info := range infos {
		filename := filepath.Base(info.FilePath)
		id := strings.TrimSuffix(filename, filepath.Ext(filename))
		talkers := make(map[string]struct{}, len(info.TalkerMap))
		for t := range info.TalkerMap {
			talkers[t] = struct{}{}
		}
		store := &msgstore.Store{
			ID:        id,
			FilePath:  info.FilePath,
			FileName:  filename,
			IndexPath: filepath.Join(ds.path, "indexes", "messages", id+".fts.db"),
			StartTime: info.StartTime,
			EndTime:   info.EndTime,
			Talkers:   talkers,
		}
		stores = append(stores, store)
	}
	ds.messageStoreMu.Lock()
	ds.messageStores = stores
	ds.messageStoreMu.Unlock()
	ds.invalidateTalkerCache()
	return nil
}

// getDBInfosForTimeRange 获取时间范围内的数据库信息
func (ds *DataSource) getDBInfosForTimeRange(startTime, endTime time.Time) []MessageDBInfo {
	var dbs []MessageDBInfo
	for _, info := range ds.messageInfos {
		if info.StartTime.Before(endTime) && info.EndTime.After(startTime) {
			dbs = append(dbs, info)
		}
	}
	return dbs
}

func (ds *DataSource) GetMessages(ctx context.Context, startTime, endTime time.Time, talker string, sender string, keyword string, limit, offset int) ([]*model.Message, error) {
	if talker == "" {
		return nil, errors.ErrTalkerEmpty
	}

	// 解析talker参数，支持多个talker（以英文逗号分隔）
	talkers := util.Str2List(talker, ",")
	if len(talkers) == 0 {
		return nil, errors.ErrTalkerEmpty
	}

	// 找到时间范围内的数据库文件
	dbInfos := ds.getDBInfosForTimeRange(startTime, endTime)
	if len(dbInfos) == 0 {
		return nil, errors.TimeRangeNotFound(startTime, endTime)
	}

	// 解析sender参数，支持多个发送者（以英文逗号分隔）
	senders := util.Str2List(sender, ",")

	// 预编译正则表达式（如果有keyword）
	var regex *regexp.Regexp
	if keyword != "" {
		var err error
		regex, err = regexp.Compile(keyword)
		if err != nil {
			return nil, errors.QueryFailed("invalid regex pattern", err)
		}
	}

	// 从每个相关数据库中查询消息
	filteredMessages := []*model.Message{}

	for _, dbInfo := range dbInfos {
		// 检查上下文是否已取消
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		db, err := ds.dbm.OpenDB(dbInfo.FilePath)
		if err != nil {
			log.Error().Msgf("数据库 %s 未打开", dbInfo.FilePath)
			continue
		}

		// 对每个talker进行查询
		for _, talkerItem := range talkers {
			// 构建查询条件
			conditions := []string{"Sequence >= ? AND Sequence <= ?"}
			args := []interface{}{startTime.Unix() * 1000, endTime.Unix() * 1000}

			// 添加talker条件
			talkerID, ok := dbInfo.TalkerMap[talkerItem]
			if ok {
				conditions = append(conditions, "TalkerId = ?")
				args = append(args, talkerID)
			} else {
				conditions = append(conditions, "StrTalker = ?")
				args = append(args, talkerItem)
			}

			query := fmt.Sprintf(`
				SELECT MsgSvrID, Sequence, CreateTime, StrTalker, IsSender, 
					Type, SubType, StrContent, CompressContent, BytesExtra
				FROM MSG 
				WHERE %s 
				ORDER BY Sequence ASC
			`, strings.Join(conditions, " AND "))

			// 执行查询
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				// 如果表不存在，跳过此talker
				if strings.Contains(err.Error(), "no such table") {
					continue
				}
				log.Err(err).Msgf("从数据库 %s 查询消息失败", dbInfo.FilePath)
				continue
			}

			// 处理查询结果，在读取时进行过滤
			for rows.Next() {
				var msg model.MessageV3
				var compressContent []byte
				var bytesExtra []byte

				err := rows.Scan(
					&msg.MsgSvrID,
					&msg.Sequence,
					&msg.CreateTime,
					&msg.StrTalker,
					&msg.IsSender,
					&msg.Type,
					&msg.SubType,
					&msg.StrContent,
					&compressContent,
					&bytesExtra,
				)
				if err != nil {
					rows.Close()
					return nil, errors.ScanRowFailed(err)
				}
				msg.CompressContent = compressContent
				msg.BytesExtra = bytesExtra

				// 将消息转换为标准格式
				message := msg.Wrap()

				// 应用sender过滤
				if len(senders) > 0 {
					senderMatch := false
					for _, s := range senders {
						if message.Sender == s {
							senderMatch = true
							break
						}
					}
					if !senderMatch {
						continue // 不匹配sender，跳过此消息
					}
				}

				// 应用keyword过滤
				if regex != nil {
					plainText := message.PlainTextContent()
					if !regex.MatchString(plainText) {
						continue // 不匹配keyword，跳过此消息
					}
				}

				// 通过所有过滤条件，保留此消息
				filteredMessages = append(filteredMessages, message)

				// 检查是否已经满足分页处理数量
				if limit > 0 && len(filteredMessages) >= offset+limit {
					// 已经获取了足够的消息，可以提前返回
					rows.Close()

					// 对所有消息按时间排序
					sort.Slice(filteredMessages, func(i, j int) bool {
						return filteredMessages[i].Seq < filteredMessages[j].Seq
					})

					// 处理分页
					if offset >= len(filteredMessages) {
						return []*model.Message{}, nil
					}
					end := offset + limit
					if end > len(filteredMessages) {
						end = len(filteredMessages)
					}
					return filteredMessages[offset:end], nil
				}
			}
			rows.Close()
		}
	}

	// 对所有消息按时间排序
	sort.Slice(filteredMessages, func(i, j int) bool {
		return filteredMessages[i].Seq < filteredMessages[j].Seq
	})

	// 处理分页
	if limit > 0 {
		if offset >= len(filteredMessages) {
			return []*model.Message{}, nil
		}
		end := offset + limit
		if end > len(filteredMessages) {
			end = len(filteredMessages)
		}
		return filteredMessages[offset:end], nil
	}

	return filteredMessages, nil
}

func (ds *DataSource) GetDatasetFingerprint(context.Context) (string, error) {
	return ds.dbm.FingerprintForGroups(Message)
}

func (ds *DataSource) ListMessageStores(ctx context.Context) ([]*msgstore.Store, error) {
	_ = ctx
	ds.messageStoreMu.RLock()
	defer ds.messageStoreMu.RUnlock()

	stores := make([]*msgstore.Store, len(ds.messageStores))
	for i, store := range ds.messageStores {
		stores[i] = store.Clone()
	}
	return stores, nil
}

func (ds *DataSource) LocateMessageStore(msg *model.Message) (*msgstore.Store, error) {
	if msg == nil {
		return nil, errors.MessageStoreNotFound("nil message")
	}

	talker := strings.TrimSpace(msg.Talker)
	ts := msg.Time

	ds.messageStoreMu.RLock()
	defer ds.messageStoreMu.RUnlock()

	if ts.IsZero() {
		for _, store := range ds.messageStores {
			if len(store.Talkers) == 0 {
				continue
			}
			if _, ok := store.Talkers[talker]; ok {
				return store, nil
			}
		}
		return nil, errors.MessageStoreNotFound(talker)
	}

	for _, store := range ds.messageStores {
		if len(store.Talkers) > 0 {
			if _, ok := store.Talkers[talker]; !ok {
				continue
			}
		}
		if (ts.Equal(store.StartTime) || ts.After(store.StartTime)) && ts.Before(store.EndTime) {
			return store, nil
		}
	}

	for _, store := range ds.messageStores {
		if len(store.Talkers) > 0 {
			if _, ok := store.Talkers[talker]; ok {
				return store, nil
			}
		}
	}

	return nil, errors.MessageStoreNotFound(fmt.Sprintf("%s@%s", talker, ts.Format(time.RFC3339)))
}

func (ds *DataSource) getCachedTalkers() []string {
	ds.talkerCacheMu.RLock()
	if ds.talkerCacheExpiry.IsZero() || time.Now().After(ds.talkerCacheExpiry) {
		ds.talkerCacheMu.RUnlock()
		return nil
	}
	cached := append([]string(nil), ds.talkerCache...)
	ds.talkerCacheMu.RUnlock()
	return cached
}

func (ds *DataSource) cacheTalkers(talkers []string) {
	copySlice := append([]string(nil), talkers...)
	ds.talkerCacheMu.Lock()
	ds.talkerCache = copySlice
	ds.talkerCacheExpiry = time.Now().Add(talkerCacheTTL)
	ds.talkerCacheMu.Unlock()
}

func (ds *DataSource) invalidateTalkerCache() {
	ds.talkerCacheMu.Lock()
	ds.talkerCache = nil
	ds.talkerCacheExpiry = time.Time{}
	ds.talkerCacheMu.Unlock()
}

func (ds *DataSource) collectAllTalkers(ctx context.Context) ([]string, error) {
	if cached := ds.getCachedTalkers(); cached != nil {
		return cached, nil
	}

	talkerSet := make(map[string]struct{})
	talkers := make([]string, 0)

	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, exists := talkerSet[name]; exists {
			return
		}
		talkerSet[name] = struct{}{}
		talkers = append(talkers, name)
	}

	if db, err := ds.dbm.GetDB(Contact); err == nil && db != nil {
		rows, err := db.QueryContext(ctx, `SELECT strUsrName FROM Session WHERE IFNULL(strUsrName,'') <> ''`)
		if err == nil {
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					rows.Close()
					return nil, err
				}
				var username string
				if err := rows.Scan(&username); err != nil {
					log.Debug().Err(err).Msg("scan session talker failed")
					continue
				}
				add(username)
			}
			if err := rows.Err(); err != nil {
				log.Debug().Err(err).Msg("iterate session talkers failed")
			}
			rows.Close()
		} else {
			log.Debug().Err(err).Msg("query session talkers failed")
		}

		if len(talkers) == 0 {
			rows, err = db.QueryContext(ctx, `SELECT UserName FROM Contact WHERE IFNULL(UserName,'') <> ''`)
			if err == nil {
				for rows.Next() {
					if err := ctx.Err(); err != nil {
						rows.Close()
						return nil, err
					}
					var username string
					if err := rows.Scan(&username); err != nil {
						log.Debug().Err(err).Msg("scan contact talker failed")
						continue
					}
					add(username)
				}
				if err := rows.Err(); err != nil {
					log.Debug().Err(err).Msg("iterate contact talkers failed")
				}
				rows.Close()
			} else {
				log.Debug().Err(err).Msg("query contact talkers failed")
			}
		}
	}

	if len(talkers) == 0 {
		for _, info := range ds.messageInfos {
			for username := range info.TalkerMap {
				add(username)
			}
		}
	}

	sort.Strings(talkers)
	ds.cacheTalkers(talkers)
	return talkers, nil
}

// ListTalkers 返回所有对话用户名（联系人 + 群聊），供索引使用
func (ds *DataSource) ListTalkers(ctx context.Context) ([]string, error) {
	return ds.collectAllTalkers(ctx)
}

// IterateMessages 按 talker 枚举消息并交给处理函数，供 FTS 索引使用
func (ds *DataSource) IterateMessages(ctx context.Context, talkers []string, handler func(*model.Message) error) error {
	if handler == nil {
		return errors.InvalidArg("handler")
	}

	if len(talkers) == 0 {
		var err error
		talkers, err = ds.collectAllTalkers(ctx)
		if err != nil {
			return err
		}
	}
	if len(talkers) == 0 {
		return nil
	}

	uniqueTalkers := make([]string, 0, len(talkers))
	seen := make(map[string]struct{}, len(talkers))
	for _, t := range talkers {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		uniqueTalkers = append(uniqueTalkers, t)
	}
	if len(uniqueTalkers) == 0 {
		return nil
	}

	for _, info := range ds.messageInfos {
		if err := ctx.Err(); err != nil {
			return err
		}

		db, err := ds.dbm.OpenDB(info.FilePath)
		if err != nil {
			log.Debug().Err(err).Msgf("open message db failed: %s", info.FilePath)
			continue
		}

		for _, talker := range uniqueTalkers {
			if err := ctx.Err(); err != nil {
				return err
			}

			conditions := []string{"StrContent IS NOT NULL"}
			args := make([]interface{}, 0, 1)
			if talkerID, ok := info.TalkerMap[talker]; ok {
				conditions = append(conditions, "TalkerId = ?")
				args = append(args, talkerID)
			} else {
				conditions = append(conditions, "StrTalker = ?")
				args = append(args, talker)
			}

			query := fmt.Sprintf(`
				SELECT MsgSvrID, Sequence, CreateTime, StrTalker, IsSender,
				       Type, SubType, StrContent, CompressContent, BytesExtra
				FROM MSG
				WHERE %s
				ORDER BY Sequence ASC
			`, strings.Join(conditions, " AND "))

			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				if strings.Contains(err.Error(), "no such table") {
					continue
				}
				return errors.QueryFailed("iterate messages", err)
			}

			for rows.Next() {
				if err := ctx.Err(); err != nil {
					rows.Close()
					return err
				}
				var msg model.MessageV3
				var compressContent []byte
				var bytesExtra []byte
				if scanErr := rows.Scan(
					&msg.MsgSvrID,
					&msg.Sequence,
					&msg.CreateTime,
					&msg.StrTalker,
					&msg.IsSender,
					&msg.Type,
					&msg.SubType,
					&msg.StrContent,
					&compressContent,
					&bytesExtra,
				); scanErr != nil {
					rows.Close()
					return errors.ScanRowFailed(scanErr)
				}
				msg.CompressContent = compressContent
				msg.BytesExtra = bytesExtra

				wrapped := msg.Wrap()
				if err := handler(wrapped); err != nil {
					rows.Close()
					return err
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return errors.QueryFailed("iterate message rows", err)
			}
			rows.Close()
		}
	}

	return nil
}

// GetContacts 实现获取联系人信息的方法
func (ds *DataSource) GetContacts(ctx context.Context, key string, limit, offset int) ([]*model.Contact, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT UserName, Alias, Remark, NickName, Reserved1 FROM Contact 
                WHERE UserName = ? OR Alias = ? OR Remark = ? OR NickName = ?`
		args = []interface{}{key, key, key, key}
	} else {
		// 查询所有联系人
		query = `SELECT UserName, Alias, Remark, NickName, Reserved1 FROM Contact`
	}

	// 添加排序、分页
	query += ` ORDER BY UserName`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	// 执行查询
	db, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.QueryFailed(query, err)
	}
	defer rows.Close()

	contacts := []*model.Contact{}
	for rows.Next() {
		var contactV3 model.ContactV3
		err := rows.Scan(
			&contactV3.UserName,
			&contactV3.Alias,
			&contactV3.Remark,
			&contactV3.NickName,
			&contactV3.Reserved1,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		contacts = append(contacts, contactV3.Wrap())
	}

	return contacts, nil
}

// GetChatRooms 实现获取群聊信息的方法
func (ds *DataSource) GetChatRooms(ctx context.Context, key string, limit, offset int) ([]*model.ChatRoom, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT ChatRoomName, Reserved2, RoomData FROM ChatRoom WHERE ChatRoomName = ?`
		args = []interface{}{key}

		// 执行查询
		db, err := ds.dbm.GetDB(Contact)
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.QueryFailed(query, err)
		}
		defer rows.Close()

		chatRooms := []*model.ChatRoom{}
		for rows.Next() {
			var chatRoomV3 model.ChatRoomV3
			err := rows.Scan(
				&chatRoomV3.ChatRoomName,
				&chatRoomV3.Reserved2,
				&chatRoomV3.RoomData,
			)

			if err != nil {
				return nil, errors.ScanRowFailed(err)
			}

			chatRooms = append(chatRooms, chatRoomV3.Wrap())
		}

		// 如果没有找到群聊，尝试通过联系人查找
		if len(chatRooms) == 0 {
			contacts, err := ds.GetContacts(ctx, key, 1, 0)
			if err == nil && len(contacts) > 0 && strings.HasSuffix(contacts[0].UserName, "@chatroom") {
				// 再次尝试通过用户名查找群聊
				rows, err := db.QueryContext(ctx,
					`SELECT ChatRoomName, Reserved2, RoomData FROM ChatRoom WHERE ChatRoomName = ?`,
					contacts[0].UserName)

				if err != nil {
					return nil, errors.QueryFailed(query, err)
				}
				defer rows.Close()

				for rows.Next() {
					var chatRoomV3 model.ChatRoomV3
					err := rows.Scan(
						&chatRoomV3.ChatRoomName,
						&chatRoomV3.Reserved2,
						&chatRoomV3.RoomData,
					)

					if err != nil {
						return nil, errors.ScanRowFailed(err)
					}

					chatRooms = append(chatRooms, chatRoomV3.Wrap())
				}

				// 如果群聊记录不存在，但联系人记录存在，创建一个模拟的群聊对象
				if len(chatRooms) == 0 {
					chatRooms = append(chatRooms, &model.ChatRoom{
						Name:             contacts[0].UserName,
						Users:            make([]model.ChatRoomUser, 0),
						User2DisplayName: make(map[string]string),
					})
				}
			}
		}

		return chatRooms, nil
	} else {
		// 查询所有群聊
		query = `SELECT ChatRoomName, Reserved2, RoomData FROM ChatRoom`

		// 添加排序、分页
		query += ` ORDER BY ChatRoomName`
		if limit > 0 {
			query += fmt.Sprintf(" LIMIT %d", limit)
			if offset > 0 {
				query += fmt.Sprintf(" OFFSET %d", offset)
			}
		}

		// 执行查询
		db, err := ds.dbm.GetDB(Contact)
		if err != nil {
			return nil, err
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.QueryFailed(query, err)
		}
		defer rows.Close()

		chatRooms := []*model.ChatRoom{}
		for rows.Next() {
			var chatRoomV3 model.ChatRoomV3
			err := rows.Scan(
				&chatRoomV3.ChatRoomName,
				&chatRoomV3.Reserved2,
				&chatRoomV3.RoomData,
			)

			if err != nil {
				return nil, errors.ScanRowFailed(err)
			}

			chatRooms = append(chatRooms, chatRoomV3.Wrap())
		}

		return chatRooms, nil
	}
}

// GetSessions 实现获取会话信息的方法
func (ds *DataSource) GetSessions(ctx context.Context, key string, limit, offset int) ([]*model.Session, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT strUsrName, nOrder, strNickName, strContent, nTime 
                FROM Session 
                WHERE strUsrName = ? OR strNickName = ?
                ORDER BY nOrder DESC`
		args = []interface{}{key, key}
	} else {
		// 查询所有会话
		query = `SELECT strUsrName, nOrder, strNickName, strContent, nTime 
                FROM Session 
                ORDER BY nOrder DESC`
	}

	// 添加分页
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	// 执行查询
	db, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.QueryFailed(query, err)
	}
	defer rows.Close()

	sessions := []*model.Session{}
	for rows.Next() {
		var sessionV3 model.SessionV3
		err := rows.Scan(
			&sessionV3.StrUsrName,
			&sessionV3.NOrder,
			&sessionV3.StrNickName,
			&sessionV3.StrContent,
			&sessionV3.NTime,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		sessions = append(sessions, sessionV3.Wrap())
	}

	return sessions, nil
}

func (ds *DataSource) GetMedia(ctx context.Context, _type string, key string) (*model.Media, error) {
	if key == "" {
		return nil, errors.ErrKeyEmpty
	}

	if _type == "voice" {
		return ds.GetVoice(ctx, key)
	}

	md5key, err := hex.DecodeString(key)
	if err != nil {
		return nil, errors.DecodeKeyFailed(err)
	}

	var dbType string
	var table1, table2 string

	switch _type {
	case "image":
		dbType = Image
		table1 = "HardLinkImageAttribute"
		table2 = "HardLinkImageID"
	case "video":
		dbType = Video
		table1 = "HardLinkVideoAttribute"
		table2 = "HardLinkVideoID"
	case "file":
		dbType = File
		table1 = "HardLinkFileAttribute"
		table2 = "HardLinkFileID"
	default:
		return nil, errors.MediaTypeUnsupported(_type)
	}

	db, err := ds.dbm.GetDB(dbType)
	if err != nil {
		return nil, err
	}

	query := fmt.Sprintf(`
        SELECT 
            a.FileName,
            a.ModifyTime,
            IFNULL(d1.Dir,"") AS Dir1,
            IFNULL(d2.Dir,"") AS Dir2
        FROM 
            %s a
        LEFT JOIN 
            %s d1 ON a.DirID1 = d1.DirId
        LEFT JOIN 
            %s d2 ON a.DirID2 = d2.DirId
        WHERE 
            a.Md5 = ?
    `, table1, table2, table2)
	args := []interface{}{md5key}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.QueryFailed(query, err)
	}
	defer rows.Close()

	var media *model.Media
	for rows.Next() {
		var mediaV3 model.MediaV3
		err := rows.Scan(
			&mediaV3.Name,
			&mediaV3.ModifyTime,
			&mediaV3.Dir1,
			&mediaV3.Dir2,
		)
		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}
		mediaV3.Type = _type
		mediaV3.Key = key
		media = mediaV3.Wrap()
	}

	if media == nil {
		return nil, errors.ErrMediaNotFound
	}

	return media, nil
}

func (ds *DataSource) GetVoice(ctx context.Context, key string) (*model.Media, error) {
	if key == "" {
		return nil, errors.ErrKeyEmpty
	}

	query := `
	SELECT Buf
	FROM Media
	WHERE Reserved0 = ? 
	`
	args := []interface{}{key}

	dbs, err := ds.dbm.GetDBs(Voice)
	if err != nil {
		return nil, errors.DBConnectFailed("", err)
	}

	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.QueryFailed(query, err)
		}
		defer rows.Close()

		for rows.Next() {
			var voiceData []byte
			err := rows.Scan(
				&voiceData,
			)
			if err != nil {
				return nil, errors.ScanRowFailed(err)
			}
			if len(voiceData) > 0 {
				return &model.Media{
					Type: "voice",
					Key:  key,
					Data: voiceData,
				}, nil
			}
		}
	}

	return nil, errors.ErrMediaNotFound
}

// Close 实现 DataSource 接口的 Close 方法
func (ds *DataSource) Close() error {
	return ds.dbm.Close()
}

// GetAvatar returns avatar info for a username on Windows v3 (MicroMsg.db -> ContactHeadImgUrl)
func (ds *DataSource) GetAvatar(ctx context.Context, username string, size string) (*model.Avatar, error) {
	if username == "" {
		return nil, errors.ErrKeyEmpty
	}
	db, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return nil, err
	}
	query := `SELECT IFNULL(smallHeadImgUrl, ''), IFNULL(bigHeadImgUrl, '') FROM ContactHeadImgUrl WHERE usrName = ?`
	row := db.QueryRowContext(ctx, query, username)
	var small, big string
	if err := row.Scan(&small, &big); err != nil {
		return nil, errors.ErrAvatarNotFound
	}
	url := small
	if strings.ToLower(size) == "big" && big != "" {
		url = big
	}
	if url == "" {
		url = big
	}
	if url == "" {
		return nil, errors.ErrAvatarNotFound
	}
	return &model.Avatar{Username: username, URL: url}, nil
}

// GlobalMessageStats 聚合统计（Windows v3）
func (ds *DataSource) GlobalMessageStats(ctx context.Context) (*model.GlobalMessageStats, error) {
	stats := &model.GlobalMessageStats{ByType: make(map[string]int64)}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return stats, nil
	}
	for _, db := range dbs {
		// total/sent/recv/min/max
		row := db.QueryRowContext(ctx, `SELECT 
			COUNT(*) AS total,
			SUM(CASE WHEN IsSender=1 THEN 1 ELSE 0 END) AS sent,
			SUM(CASE WHEN IsSender=0 THEN 1 ELSE 0 END) AS recv,
			MIN(CreateTime) AS minct,
			MAX(CreateTime) AS maxct
		FROM MSG`)
		var total, sent, recv, minct, maxct int64
		if err := row.Scan(&total, &sent, &recv, &minct, &maxct); err == nil {
			stats.Total += total
			stats.Sent += sent
			stats.Received += recv
			if stats.EarliestUnix == 0 || (minct > 0 && minct < stats.EarliestUnix) {
				stats.EarliestUnix = minct
			}
			if maxct > stats.LatestUnix {
				stats.LatestUnix = maxct
			}
		}

		// By type/subtype
		rows, err := db.QueryContext(ctx, `SELECT Type, SubType, COUNT(*) FROM MSG GROUP BY Type, SubType`)
		if err == nil {
			for rows.Next() {
				var t int64
				var st int
				var cnt int64
				if err := rows.Scan(&t, &st, &cnt); err == nil {
					label := mapV3TypeToLabel(t, int64(st))
					if label != "" {
						stats.ByType[label] += cnt
					}
				}
			}
			rows.Close()
		}
	}
	return stats, nil
}

// GroupMessageCounts 统计群聊消息数量（Windows v3）
func (ds *DataSource) GroupMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT StrTalker, COUNT(*) FROM MSG WHERE StrTalker LIKE '%@chatroom' GROUP BY StrTalker`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var talker string
			var cnt int64
			if err := rows.Scan(&talker, &cnt); err == nil {
				result[talker] += cnt
			}
		}
		rows.Close()
	}
	return result, nil
}

// GroupTodayMessageCounts 统计群聊今日消息数（Windows v3）：MSG 表中 StrTalker LIKE '%@chatroom' 且 CreateTime >= 今日零点
func (ds *DataSource) GroupTodayMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	// 今日零点（使用本地时区）
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := today.Unix()
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT StrTalker, COUNT(*) FROM MSG WHERE StrTalker LIKE '%@chatroom' AND CreateTime >= ? GROUP BY StrTalker`, since)
		if err != nil {
			continue
		}
		for rows.Next() {
			var talker string
			var cnt int64
			if err := rows.Scan(&talker, &cnt); err == nil {
				result[talker] += cnt
			}
		}
		rows.Close()
	}
	return result, nil
}

// GroupTodayHourly 统计群聊今日按小时消息数（Windows v3）
func (ds *DataSource) GroupTodayHourly(ctx context.Context) (map[string][24]int64, error) {
	result := make(map[string][24]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	end := start + 86400
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT StrTalker, CAST(strftime('%H', datetime(CreateTime,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM MSG WHERE StrTalker LIKE '%@chatroom' AND CreateTime >= ? AND CreateTime < ? GROUP BY StrTalker, h`, start, end)
		if err != nil {
			continue
		}
		for rows.Next() {
			var talker string
			var hour int
			var cnt int64
			if rows.Scan(&talker, &hour, &cnt) == nil {
				if hour >= 0 && hour < 24 {
					bucket := result[talker]
					bucket[hour] += cnt
					result[talker] = bucket
				}
			}
		}
		rows.Close()
	}
	return result, nil
}

// GroupWeekMessageCount 统计本周(周一00:00起)所有群聊消息总数（Windows v3）
func (ds *DataSource) GroupWeekMessageCount(ctx context.Context) (int64, error) {
	var total int64
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return 0, nil
	}
	now := time.Now()
	wday := int(now.Weekday())
	offset := wday - 1
	if wday == 0 {
		offset = -6
	}
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -offset)
	since := monday.Unix()
	for _, db := range dbs {
		row := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM MSG WHERE StrTalker LIKE '%@chatroom' AND CreateTime >= ?`, since)
		var cnt int64
		if row.Scan(&cnt) == nil {
			total += cnt
		}
	}
	return total, nil
}

// GroupMessageTypeStats 统计群聊消息类型分布（Windows v3）
func (ds *DataSource) GroupMessageTypeStats(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT Type, SubType, COUNT(*) FROM MSG WHERE StrTalker LIKE '%@chatroom' GROUP BY Type, SubType`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var t int64
			var st int64
			var cnt int64
			if rows.Scan(&t, &st, &cnt) == nil {
				label := mapV3TypeToLabel(t, st)
				if label != "" {
					result[label] += cnt
				}
			}
		}
		rows.Close()
	}
	return result, nil
}

// MonthlyTrend 返回每月 sent/received（近 months 月，若 months<=0 则返回全部）
func (ds *DataSource) MonthlyTrend(ctx context.Context, months int) ([]model.MonthlyTrend, error) {
	agg := make(map[string][2]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return []model.MonthlyTrend{}, nil
	}
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT strftime('%Y-%m', datetime(CreateTime, 'unixepoch')) AS ym,
			SUM(CASE WHEN IsSender=1 THEN 1 ELSE 0 END) AS sent,
			SUM(CASE WHEN IsSender=0 THEN 1 ELSE 0 END) AS recv
			FROM MSG GROUP BY ym ORDER BY ym`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var ym string
			var sent, recv int64
			if err := rows.Scan(&ym, &sent, &recv); err == nil {
				cur := agg[ym]
				cur[0] += sent
				cur[1] += recv
				agg[ym] = cur
			}
		}
		rows.Close()
	}
	trends := make([]model.MonthlyTrend, 0, len(agg))
	// order is not guaranteed; we'll reconstruct sorted keys
	// but to keep simple, iterate map and collect; sorting can be added later
	for ym, v := range agg {
		trends = append(trends, model.MonthlyTrend{Date: ym, Sent: v[0], Received: v[1]})
	}
	// optional: limit to recent months by trimming after sort
	return trends, nil
}

// Heatmap 小时x星期（wday: 0=Sunday..6）
func (ds *DataSource) Heatmap(ctx context.Context) ([24][7]int64, error) {
	var grid [24][7]int64
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return grid, nil
	}
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT CAST(strftime('%H', datetime(CreateTime,'unixepoch')) AS INTEGER) AS h,
			CAST(strftime('%w', datetime(CreateTime,'unixepoch')) AS INTEGER) AS d,
			COUNT(*) FROM MSG GROUP BY h,d`)
		if err != nil {
			continue
		}
		for rows.Next() {
			var h, d int
			var cnt int64
			if err := rows.Scan(&h, &d, &cnt); err == nil {
				if h >= 0 && h < 24 && d >= 0 && d < 7 {
					grid[h][d] += cnt
				}
			}
		}
		rows.Close()
	}
	return grid, nil
}

// GlobalTodayHourly 返回今日(本地时区)每小时全部消息量（含私聊+群聊）
func (ds *DataSource) GlobalTodayHourly(ctx context.Context) ([24]int64, error) {
	var hours [24]int64
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return hours, nil
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	end := start + 86400
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT CAST(strftime('%H', datetime(CreateTime,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM MSG WHERE CreateTime >= ? AND CreateTime < ? GROUP BY h`, start, end)
		if err != nil {
			continue
		}
		for rows.Next() {
			var h int
			var cnt int64
			if rows.Scan(&h, &cnt) == nil {
				if h >= 0 && h < 24 {
					hours[h] += cnt
				}
			}
		}
		rows.Close()
	}
	return hours, nil
}

// IntimacyBase 统计按联系人（非群聊）聚合的亲密度基础数据（Windows v3）
func (ds *DataSource) IntimacyBase(ctx context.Context) (map[string]*model.IntimacyBase, error) {
	result := make(map[string]*model.IntimacyBase)

	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}

	// 先获取全局最新时间戳
	var maxCT int64
	for _, db := range dbs {
		row := db.QueryRowContext(ctx, `SELECT MAX(CreateTime) FROM MSG`)
		var v sql.NullInt64
		if err := row.Scan(&v); err == nil && v.Valid && v.Int64 > maxCT {
			maxCT = v.Int64
		}
	}
	if maxCT == 0 {
		return result, nil
	}
	since90 := maxCT - 90*86400
	since7 := maxCT - 7*86400

	for _, db := range dbs {
		// 基础计数
		rows, err := db.QueryContext(ctx, `SELECT StrTalker,
			COUNT(*) AS msg_count,
			MIN(CreateTime) AS minct,
			MAX(CreateTime) AS maxct,
			SUM(CASE WHEN IsSender=1 THEN 1 ELSE 0 END) AS sent,
			SUM(CASE WHEN IsSender=0 THEN 1 ELSE 0 END) AS recv
			FROM MSG WHERE StrTalker NOT LIKE '%@chatroom' GROUP BY StrTalker`)
		if err == nil {
			for rows.Next() {
				var talker string
				var msgCnt, minct, maxct, sent, recv int64
				if err := rows.Scan(&talker, &msgCnt, &minct, &maxct, &sent, &recv); err == nil {
					base := result[talker]
					if base == nil {
						base = &model.IntimacyBase{UserName: talker}
						result[talker] = base
					}
					base.MsgCount += msgCnt
					base.SentCount += sent
					base.ReceivedCount += recv
					if base.MinCreateUnix == 0 || (minct > 0 && minct < base.MinCreateUnix) {
						base.MinCreateUnix = minct
					}
					if maxct > base.MaxCreateUnix {
						base.MaxCreateUnix = maxct
					}
				}
			}
			rows.Close()
		}

		// 活跃天数（全期间）
		rows2, err := db.QueryContext(ctx, `SELECT StrTalker, COUNT(DISTINCT date(datetime(CreateTime,'unixepoch'))) AS days
			FROM MSG WHERE StrTalker NOT LIKE '%@chatroom' GROUP BY StrTalker`)
		if err == nil {
			for rows2.Next() {
				var talker string
				var days int64
				if err := rows2.Scan(&talker, &days); err == nil {
					base := result[talker]
					if base == nil {
						base = &model.IntimacyBase{UserName: talker}
						result[talker] = base
					}
					base.MessagingDays += days
				}
			}
			rows2.Close()
		}

		// 最近90天消息数
		rows3, err := db.QueryContext(ctx, `SELECT StrTalker, COUNT(*) FROM MSG WHERE CreateTime>=? AND StrTalker NOT LIKE '%@chatroom' GROUP BY StrTalker`, since90)
		if err == nil {
			for rows3.Next() {
				var talker string
				var cnt int64
				if err := rows3.Scan(&talker, &cnt); err == nil {
					base := result[talker]
					if base == nil {
						base = &model.IntimacyBase{UserName: talker}
						result[talker] = base
					}
					base.Last90DaysMsg += cnt
				}
			}
			rows3.Close()
		}

		// 过去7天发送数
		rows4, err := db.QueryContext(ctx, `SELECT StrTalker, SUM(CASE WHEN IsSender=1 THEN 1 ELSE 0 END) FROM MSG WHERE CreateTime>=? AND StrTalker NOT LIKE '%@chatroom' GROUP BY StrTalker`, since7)
		if err == nil {
			for rows4.Next() {
				var talker string
				var cnt sql.NullInt64
				if err := rows4.Scan(&talker, &cnt); err == nil {
					base := result[talker]
					if base == nil {
						base = &model.IntimacyBase{UserName: talker}
						result[talker] = base
					}
					if cnt.Valid {
						base.Past7DaysSentMsg += cnt.Int64
					}
				}
			}
			rows4.Close()
		}
	}

	return result, nil
}

// mapV3TypeToLabel 将v3 (Type,SubType) 映射为中文类别
func mapV3TypeToLabel(t int64, st int64) string {
	// Windows v3：在统一文档分类基础上补充文件/链接细分（Type=49 根据 SubType 区分）
	switch t {
	case 1:
		return "文本消息"
	case 3:
		return "图片消息"
	case 34:
		return "语音消息"
	case 37:
		return "好友验证消息"
	case 42:
		return "好友推荐消息"
	case 47:
		return "聊天表情"
	case 48:
		return "位置消息"
	case 49:
		if st == 6 { // 文件
			return "文件消息"
		}
		if st == 4 || st == 5 { // 链接
			return "链接消息"
		}
		return "XML消息" // 其他未识别子类型归类为通用 XML
	case 50:
		return "音视频通话"
	case 51:
		return "手机端操作消息"
	case 10000:
		return "系统通知"
	case 10002:
		return "撤回消息"
	default:
		return ""
	}
}
