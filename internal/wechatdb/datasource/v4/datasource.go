package v4

import (
	"context"
	"crypto/md5"
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
	Session = "session"
	Media   = "media"
	Voice   = "voice"
)

var Groups = []*dbm.Group{
	{
		Name:      Message,
		Pattern:   `^message_([0-9]?[0-9])?\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Contact,
		Pattern:   `^contact\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Session,
		Pattern:   `session\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Media,
		Pattern:   `^hardlink\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Voice,
		Pattern:   `^media_([0-9]?[0-9])?\.db$`,
		BlackList: []string{},
	},
	{
		Name:      "headimg",
		Pattern:   `^head_image\.db$`,
		BlackList: []string{},
	},
}

// MessageDBInfo 存储消息数据库的信息
type MessageDBInfo struct {
	FilePath  string
	StartTime time.Time
	EndTime   time.Time
}

type DataSource struct {
	path string
	dbm  *dbm.DBManager

	// 消息数据库信息
	messageInfos []MessageDBInfo

	talkerDBMap        map[string]string
	messageStores      []*msgstore.Store
	messageStoreByPath map[string]*msgstore.Store
	messageStoreMu     sync.RWMutex
}

func New(path string) (*DataSource, error) {

	ds := &DataSource{
		path:               path,
		dbm:                dbm.NewDBManager(path),
		messageInfos:       make([]MessageDBInfo, 0),
		talkerDBMap:        make(map[string]string),
		messageStores:      make([]*msgstore.Store, 0),
		messageStoreByPath: make(map[string]*msgstore.Store),
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
		if !event.Op.Has(fsnotify.Create) {
			return nil
		}
		if err := ds.initMessageDbs(); err != nil {
			log.Err(err).Msgf("Failed to reinitialize message DBs: %s", event.Name)
		}
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
	ds.messageStoreMu.RLock()
	defer ds.messageStoreMu.RUnlock()

	if talker != "" {
		hash := md5.Sum([]byte(talker))
		key := hex.EncodeToString(hash[:])
		if path, ok := ds.talkerDBMap[key]; ok {
			if store, exists := ds.messageStoreByPath[path]; exists {
				return store, nil
			}
		}
	}

	ts := msg.Time
	if !ts.IsZero() {
		for _, store := range ds.messageStores {
			if (ts.Equal(store.StartTime) || ts.After(store.StartTime)) && ts.Before(store.EndTime) {
				return store, nil
			}
		}
	}

	if talker == "" {
		talker = "unknown"
	}
	return nil, errors.MessageStoreNotFound(talker)
}

func (ds *DataSource) initMessageDbs() error {
	dbPaths, err := ds.dbm.GetDBPath(Message)
	if err != nil {
		if strings.Contains(err.Error(), "db file not found") {
			ds.messageInfos = make([]MessageDBInfo, 0)
			return nil
		}
		return err
	}

	// 处理每个数据库文件
	infos := make([]MessageDBInfo, 0)
	talkerDBMap := make(map[string]string)
	talkerSets := make(map[string]map[string]struct{})
	for _, filePath := range dbPaths {
		db, err := ds.dbm.OpenDB(filePath)
		if err != nil {
			log.Err(err).Msgf("获取数据库 %s 失败", filePath)
			continue
		}

		talkers := make(map[string]struct{})
		talkerSets[filePath] = talkers

		// 获取 Timestamp 表中的开始时间
		var startTime time.Time
		var timestamp int64

		row := db.QueryRow("SELECT timestamp FROM Timestamp LIMIT 1")
		if err := row.Scan(&timestamp); err != nil {
			log.Err(err).Msgf("获取数据库 %s 的时间戳失败", filePath)
			continue
		}
		startTime = time.Unix(timestamp, 0)

		rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'")
		if err != nil {
			log.Debug().Err(err).Msgf("数据库 %s 查询 Msg 表失败", filePath)
		} else {
			for rows.Next() {
				var tableName string
				if err := rows.Scan(&tableName); err != nil {
					log.Debug().Err(err).Msgf("数据库 %s 扫描 Msg 表失败", filePath)
					continue
				}

				hash := strings.TrimPrefix(tableName, "Msg_")
				if hash == "" {
					continue
				}
				talkers[hash] = struct{}{}
				talkerDBMap[hash] = filePath
			}
			rows.Close()
		}

		// 保存数据库信息
		infos = append(infos, MessageDBInfo{
			FilePath:  filePath,
			StartTime: startTime,
		})
	}

	// 按照 StartTime 排序数据库文件
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].StartTime.Before(infos[j].StartTime)
	})

	// 设置结束时间
	for i := range infos {
		if i == len(infos)-1 {
			infos[i].EndTime = time.Now().Add(time.Hour)
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
	storeByPath := make(map[string]*msgstore.Store, len(infos))
	for _, info := range infos {
		filename := filepath.Base(info.FilePath)
		id := strings.TrimSuffix(filename, filepath.Ext(filename))
		var talkerMap map[string]struct{}
		if set := talkerSets[info.FilePath]; len(set) > 0 {
			talkerMap = make(map[string]struct{}, len(set))
			for hash := range set {
				talkerMap[hash] = struct{}{}
			}
		}
		store := &msgstore.Store{
			ID:        id,
			FilePath:  info.FilePath,
			FileName:  filename,
			IndexPath: filepath.Join(ds.path, "indexes", "messages", id+".fts.db"),
			StartTime: info.StartTime,
			EndTime:   info.EndTime,
			Talkers:   talkerMap,
		}
		stores = append(stores, store)
		storeByPath[info.FilePath] = store
	}

	ds.messageStoreMu.Lock()
	ds.messageStores = stores
	ds.messageStoreByPath = storeByPath
	ds.messageStoreMu.Unlock()
	ds.talkerDBMap = talkerDBMap
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

	// 从每个相关数据库中查询消息，并在读取时进行过滤
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
			// 构建表名
			_talkerMd5Bytes := md5.Sum([]byte(talkerItem))
			talkerMd5 := hex.EncodeToString(_talkerMd5Bytes[:])
			tableName := "Msg_" + talkerMd5

			// 检查表是否存在
			var exists bool
			err = db.QueryRowContext(ctx,
				"SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
				tableName).Scan(&exists)

			if err != nil {
				if err == sql.ErrNoRows {
					// 表不存在，继续下一个talker
					continue
				}
				return nil, errors.QueryFailed("", err)
			}

			// 构建查询条件
			conditions := []string{"create_time >= ? AND create_time <= ?"}
			args := []interface{}{startTime.Unix(), endTime.Unix()}
			log.Debug().Msgf("Table name: %s", tableName)
			log.Debug().Msgf("Start time: %d, End time: %d", startTime.Unix(), endTime.Unix())

			query := fmt.Sprintf(`
				SELECT m.sort_seq, m.server_id, m.local_type, n.user_name, m.create_time, m.message_content, m.packed_info_data, m.status
				FROM %s m
				LEFT JOIN Name2Id n ON m.real_sender_id = n.rowid
				WHERE %s 
				ORDER BY m.sort_seq ASC
			`, tableName, strings.Join(conditions, " AND "))

			// 执行查询
			rows, err := db.QueryContext(ctx, query, args...)
			if err != nil {
				// 如果表不存在，SQLite 会返回错误
				if strings.Contains(err.Error(), "no such table") {
					continue
				}
				log.Err(err).Msgf("从数据库 %s 查询消息失败", dbInfo.FilePath)
				continue
			}

			// 处理查询结果，在读取时进行过滤
			for rows.Next() {
				var msg model.MessageV4
				err := rows.Scan(
					&msg.SortSeq,
					&msg.ServerID,
					&msg.LocalType,
					&msg.UserName,
					&msg.CreateTime,
					&msg.MessageContent,
					&msg.PackedInfoData,
					&msg.Status,
				)
				if err != nil {
					rows.Close()
					return nil, errors.ScanRowFailed(err)
				}

				// 将消息转换为标准格式
				message := msg.Wrap(talkerItem)

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

func (ds *DataSource) ListTalkers(ctx context.Context) ([]string, error) {
	talkerSet := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := talkerSet[name]; ok {
			return
		}
		talkerSet[name] = struct{}{}
	}

	db, err := ds.dbm.GetDB(Contact)
	if err != nil {
		log.Debug().Err(err).Msg("query contact usernames failed")
	} else if db != nil {
		rows, err := db.QueryContext(ctx, `SELECT username FROM contact WHERE IFNULL(username,'') <> ''`)
		if err == nil {
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					rows.Close()
					return nil, err
				}
				var username string
				if scanErr := rows.Scan(&username); scanErr == nil {
					add(username)
				} else {
					log.Debug().Err(scanErr).Msg("scan contact username failed")
				}
			}
			if err := rows.Err(); err != nil {
				log.Debug().Err(err).Msg("iterate contact usernames failed")
			}
			rows.Close()
		} else {
			log.Debug().Err(err).Msg("query contact usernames failed")
		}

		roomRows, err := db.QueryContext(ctx, `SELECT username FROM chat_room WHERE IFNULL(username,'') <> ''`)
		if err == nil {
			for roomRows.Next() {
				if err := ctx.Err(); err != nil {
					roomRows.Close()
					return nil, err
				}
				var username string
				if scanErr := roomRows.Scan(&username); scanErr == nil {
					add(username)
				} else {
					log.Debug().Err(scanErr).Msg("scan chat_room username failed")
				}
			}
			if err := roomRows.Err(); err != nil {
				log.Debug().Err(err).Msg("iterate chat_room usernames failed")
			}
			roomRows.Close()
		} else {
			log.Debug().Err(err).Msg("query chat_room usernames failed")
		}
	}

	db, err = ds.dbm.GetDB(Session)
	if err != nil {
		log.Debug().Err(err).Msg("query session usernames failed")
	} else if db != nil {
		rows, err := db.QueryContext(ctx, `SELECT username FROM SessionTable WHERE IFNULL(username,'') <> ''`)
		if err == nil {
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					rows.Close()
					return nil, err
				}
				var username string
				if scanErr := rows.Scan(&username); scanErr == nil {
					add(username)
				} else {
					log.Debug().Err(scanErr).Msg("scan session username failed")
				}
			}
			if err := rows.Err(); err != nil {
				log.Debug().Err(err).Msg("iterate session usernames failed")
			}
			rows.Close()
		} else {
			log.Debug().Err(err).Msg("query session usernames failed")
		}
	}

	talkers := make([]string, 0, len(talkerSet))
	for username := range talkerSet {
		talkers = append(talkers, username)
	}
	sort.Strings(talkers)
	return talkers, nil
}

func (ds *DataSource) IterateMessages(ctx context.Context, talkers []string, handler func(*model.Message) error) error {
	if handler == nil {
		return errors.InvalidArg("handler")
	}

	if len(talkers) == 0 {
		var err error
		talkers, err = ds.ListTalkers(ctx)
		if err != nil {
			return err
		}
	}
	if len(talkers) == 0 {
		return nil
	}

	tableNames := make(map[string]string, len(talkers))
	for _, talker := range talkers {
		hash := md5.Sum([]byte(talker))
		tableNames[talker] = "Msg_" + hex.EncodeToString(hash[:])
	}

	for _, info := range ds.messageInfos {
		if err := ctx.Err(); err != nil {
			return err
		}

		db, err := ds.dbm.OpenDB(info.FilePath)
		if err != nil {
			continue
		}

		for _, talker := range talkers {
			if err := ctx.Err(); err != nil {
				return err
			}
			tableName := tableNames[talker]

			query := fmt.Sprintf(`
				SELECT m.sort_seq, m.server_id, m.local_type, n.user_name,
				       m.create_time, m.message_content, m.packed_info_data, m.status
				FROM %s AS m
				LEFT JOIN Name2Id n ON m.real_sender_id = n.rowid
				ORDER BY m.sort_seq ASC
			`, tableName)

			rows, err := db.QueryContext(ctx, query)
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
				var msg model.MessageV4
				var messageContent []byte
				if scanErr := rows.Scan(
					&msg.SortSeq,
					&msg.ServerID,
					&msg.LocalType,
					&msg.UserName,
					&msg.CreateTime,
					&messageContent,
					&msg.PackedInfoData,
					&msg.Status,
				); scanErr != nil {
					rows.Close()
					return errors.ScanRowFailed(scanErr)
				}
				msg.MessageContent = messageContent
				message := msg.Wrap(talker)
				if err := handler(message); err != nil {
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

func (ds *DataSource) GetDatasetFingerprint(context.Context) (string, error) {
	return ds.dbm.FingerprintForGroups(Message)
}

// 联系人
func (ds *DataSource) GetContacts(ctx context.Context, key string, limit, offset int) ([]*model.Contact, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT username, local_type, alias, remark, nick_name 
				FROM contact 
				WHERE username = ? OR alias = ? OR remark = ? OR nick_name = ?`
		args = []interface{}{key, key, key, key}
	} else {
		// 查询所有联系人
		query = `SELECT username, local_type, alias, remark, nick_name FROM contact`
	}

	// 添加排序、分页
	query += ` ORDER BY username`
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
		var contactV4 model.ContactV4
		err := rows.Scan(
			&contactV4.UserName,
			&contactV4.LocalType,
			&contactV4.Alias,
			&contactV4.Remark,
			&contactV4.NickName,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		contacts = append(contacts, contactV4.Wrap())
	}

	return contacts, nil
}

// 群聊
func (ds *DataSource) GetChatRooms(ctx context.Context, key string, limit, offset int) ([]*model.ChatRoom, error) {
	var query string
	var args []interface{}

	// 执行查询
	db, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return nil, err
	}

	if key != "" {
		// 按照关键字查询
		query = `SELECT username, owner, ext_buffer FROM chat_room WHERE username = ?`
		args = []interface{}{key}

		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.QueryFailed(query, err)
		}
		defer rows.Close()

		chatRooms := []*model.ChatRoom{}
		for rows.Next() {
			var chatRoomV4 model.ChatRoomV4
			err := rows.Scan(
				&chatRoomV4.UserName,
				&chatRoomV4.Owner,
				&chatRoomV4.ExtBuffer,
			)

			if err != nil {
				return nil, errors.ScanRowFailed(err)
			}

			chatRooms = append(chatRooms, chatRoomV4.Wrap())
		}

		// 如果没有找到群聊，尝试通过联系人查找
		if len(chatRooms) == 0 {
			contacts, err := ds.GetContacts(ctx, key, 1, 0)
			if err == nil && len(contacts) > 0 && strings.HasSuffix(contacts[0].UserName, "@chatroom") {
				// 再次尝试通过用户名查找群聊
				rows, err := db.QueryContext(ctx,
					`SELECT username, owner, ext_buffer FROM chat_room WHERE username = ?`,
					contacts[0].UserName)

				if err != nil {
					return nil, errors.QueryFailed(query, err)
				}
				defer rows.Close()

				for rows.Next() {
					var chatRoomV4 model.ChatRoomV4
					err := rows.Scan(
						&chatRoomV4.UserName,
						&chatRoomV4.Owner,
						&chatRoomV4.ExtBuffer,
					)

					if err != nil {
						return nil, errors.ScanRowFailed(err)
					}

					chatRooms = append(chatRooms, chatRoomV4.Wrap())
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
		query = `SELECT username, owner, ext_buffer FROM chat_room`

		// 添加排序、分页
		query += ` ORDER BY username`
		if limit > 0 {
			query += fmt.Sprintf(" LIMIT %d", limit)
			if offset > 0 {
				query += fmt.Sprintf(" OFFSET %d", offset)
			}
		}

		// 执行查询
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, errors.QueryFailed(query, err)
		}
		defer rows.Close()

		chatRooms := []*model.ChatRoom{}
		for rows.Next() {
			var chatRoomV4 model.ChatRoomV4
			err := rows.Scan(
				&chatRoomV4.UserName,
				&chatRoomV4.Owner,
				&chatRoomV4.ExtBuffer,
			)

			if err != nil {
				return nil, errors.ScanRowFailed(err)
			}

			chatRooms = append(chatRooms, chatRoomV4.Wrap())
		}

		return chatRooms, nil
	}
}

// 最近会话
func (ds *DataSource) GetSessions(ctx context.Context, key string, limit, offset int) ([]*model.Session, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT username, summary, last_timestamp, last_msg_sender, last_sender_display_name 
				FROM SessionTable 
				WHERE username = ? OR last_sender_display_name = ?
				ORDER BY sort_timestamp DESC`
		args = []interface{}{key, key}
	} else {
		// 查询所有会话
		query = `SELECT username, summary, last_timestamp, last_msg_sender, last_sender_display_name 
				FROM SessionTable 
				ORDER BY sort_timestamp DESC`
	}

	// 添加分页
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	// 执行查询
	db, err := ds.dbm.GetDB(Session)
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
		var sessionV4 model.SessionV4
		err := rows.Scan(
			&sessionV4.Username,
			&sessionV4.Summary,
			&sessionV4.LastTimestamp,
			&sessionV4.LastMsgSender,
			&sessionV4.LastSenderDisplayName,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		sessions = append(sessions, sessionV4.Wrap())
	}

	return sessions, nil
}

func (ds *DataSource) GetMedia(ctx context.Context, _type string, key string) (*model.Media, error) {
	if key == "" {
		return nil, errors.ErrKeyEmpty
	}

	var table string
	switch _type {
	case "image":
		table = "image_hardlink_info_v3"
		// 4.1.0 版本开始使用 v4 表
		if !ds.IsExist(Media, table) {
			table = "image_hardlink_info_v4"
		}
	case "video":
		table = "video_hardlink_info_v3"
		if !ds.IsExist(Media, table) {
			table = "video_hardlink_info_v4"
		}
	case "file":
		table = "file_hardlink_info_v3"
		if !ds.IsExist(Media, table) {
			table = "file_hardlink_info_v4"
		}
	case "voice":
		return ds.GetVoice(ctx, key)
	default:
		return nil, errors.MediaTypeUnsupported(_type)
	}

	query := fmt.Sprintf(`
	SELECT 
		f.md5,
		f.file_name,
		f.file_size,
		f.modify_time,
		IFNULL(d1.username,""),
		IFNULL(d2.username,"")
	FROM 
		%s f
	LEFT JOIN 
		dir2id d1 ON d1.rowid = f.dir1
	LEFT JOIN 
		dir2id d2 ON d2.rowid = f.dir2
	`, table)
	query += " WHERE f.md5 = ? OR f.file_name LIKE ? || '%'"
	args := []interface{}{key, key}

	// 执行查询
	db, err := ds.dbm.GetDB(Media)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.QueryFailed(query, err)
	}
	defer rows.Close()

	var media *model.Media
	for rows.Next() {
		var mediaV4 model.MediaV4
		err := rows.Scan(
			&mediaV4.Key,
			&mediaV4.Name,
			&mediaV4.Size,
			&mediaV4.ModifyTime,
			&mediaV4.Dir1,
			&mediaV4.Dir2,
		)
		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}
		mediaV4.Type = _type
		media = mediaV4.Wrap()

		// 优先返回高清图
		if _type == "image" && strings.HasSuffix(mediaV4.Name, "_h.dat") {
			break
		}
	}

	if media == nil {
		return nil, errors.ErrMediaNotFound
	}

	return media, nil
}

func (ds *DataSource) IsExist(_db string, table string) bool {
	db, err := ds.dbm.GetDB(_db)
	if err != nil {
		return false
	}
	var tableName string
	query := "SELECT name FROM sqlite_master WHERE type='table' AND name=?;"
	if err = db.QueryRow(query, table).Scan(&tableName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		return false
	}
	return true
}

func (ds *DataSource) GetVoice(ctx context.Context, key string) (*model.Media, error) {
	if key == "" {
		return nil, errors.ErrKeyEmpty
	}

	query := `
	SELECT voice_data
	FROM VoiceInfo
	WHERE svr_id = ? 
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

func (ds *DataSource) Close() error {
	return ds.dbm.Close()
}

// GetAvatar for v4: read head_image.db -> head_image(username, image_buffer)
func (ds *DataSource) GetAvatar(ctx context.Context, username string, size string) (*model.Avatar, error) {
	if username == "" {
		return nil, errors.ErrKeyEmpty
	}
	// open head_image db
	db, err := ds.dbm.GetDB("headimg")
	if err != nil {
		return nil, errors.ErrAvatarNotFound
	}
	// table may be head_image, columns: username, image_buffer (jfif), update_time, md5
	row := db.QueryRowContext(ctx, `SELECT image_buffer FROM head_image WHERE username = ?`, username)
	var buf []byte
	if err := row.Scan(&buf); err != nil || len(buf) == 0 {
		return nil, errors.ErrAvatarNotFound
	}
	return &model.Avatar{Username: username, ContentType: "image/jpeg", Data: buf}, nil
}

// GlobalMessageStats 聚合统计（Windows/Darwin v4）
func (ds *DataSource) GlobalMessageStats(ctx context.Context) (*model.GlobalMessageStats, error) {
	stats := &model.GlobalMessageStats{ByType: make(map[string]int64)}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return stats, nil
	}
	for _, db := range dbs {
		// 列举所有 Msg_ 前缀表
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
		if err != nil {
			continue
		}
		var tables []string
		for trows.Next() {
			var name string
			if err := trows.Scan(&name); err == nil {
				tables = append(tables, name)
			}
		}
		trows.Close()
		for _, tbl := range tables {
			// total/sent/min/max
			q := fmt.Sprintf(`SELECT COUNT(*) AS total,
				SUM(CASE WHEN status=2 THEN 1 ELSE 0 END) AS sent,
				MIN(create_time) AS minct,
				MAX(create_time) AS maxct FROM %s`, tbl)
			row := db.QueryRowContext(ctx, q)
			var total, sent, minct, maxct int64
			if err := row.Scan(&total, &sent, &minct, &maxct); err == nil {
				stats.Total += total
				stats.Sent += sent
				stats.Received += (total - sent)
				if stats.EarliestUnix == 0 || (minct > 0 && minct < stats.EarliestUnix) {
					stats.EarliestUnix = minct
				}
				if maxct > stats.LatestUnix {
					stats.LatestUnix = maxct
				}
			}
			// by type (细分 49)
			// 先统计除 49 之外类型
			q2 := fmt.Sprintf(`SELECT local_type, COUNT(*) FROM %s WHERE local_type != 49 GROUP BY local_type`, tbl)
			rows, err := db.QueryContext(ctx, q2)
			if err == nil {
				for rows.Next() {
					var t int64
					var cnt int64
					if err := rows.Scan(&t, &cnt); err == nil {
						label := mapV4TypeToLabel(t)
						if label != "" {
							stats.ByType[label] += cnt
						}
					}
				}
				rows.Close()
			}
			// 针对 49 类型再做细分：简单解析 message_content 判断是文件、链接或通用 XML
			q49 := fmt.Sprintf(`SELECT message_content FROM %s WHERE local_type = 49`, tbl)
			orows, err := db.QueryContext(ctx, q49)
			if err == nil {
				for orows.Next() {
					var mc []byte
					if err := orows.Scan(&mc); err == nil {
						content := string(mc)
						// 可能压缩，简单特征判断（保持轻量；深度解压需额外性能，可后续拓展）
						lc := strings.ToLower(content)
						if strings.Contains(lc, "<appmsg") {
							if strings.Contains(lc, "<type>") && strings.Contains(lc, "</type>") {
								// 简单提取 type 数字
								i1 := strings.Index(lc, "<type>")
								i2 := strings.Index(lc[i1+6:], "</type>")
								if i1 >= 0 && i2 > 0 {
									val := lc[i1+6 : i1+6+i2]
									// 常见：6=文件, 5/33=链接(网页), 3=音乐, 4=视频, 其他归类为 XML
									if strings.TrimSpace(val) == "6" {
										stats.ByType["文件消息"]++
										continue
									}
									if strings.TrimSpace(val) == "5" || strings.TrimSpace(val) == "33" {
										stats.ByType["链接消息"]++
										continue
									}
								}
							}
							// 兜底：若包含 url 或 http(s) 关键词也认为链接
							if strings.Contains(lc, "http://") || strings.Contains(lc, "https://") {
								stats.ByType["链接消息"]++
								continue
							}
							// 再兜底为 XML消息
							stats.ByType["XML消息"]++
						}
					}
				}
				orows.Close()
			}
		}
	}
	return stats, nil
}

// GroupMessageCounts 统计群聊消息数量（v4）：通过 chat_room.username 计算 md5 映射到 Msg_ 表
func (ds *DataSource) GroupMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	cdb, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return result, nil
	}
	// 获取所有群聊用户名
	urows, err := cdb.QueryContext(ctx, `SELECT username FROM chat_room`)
	if err != nil {
		return result, nil
	}
	var rooms []string
	for urows.Next() {
		var u string
		if err := urows.Scan(&u); err == nil {
			rooms = append(rooms, u)
		}
	}
	urows.Close()
	if len(rooms) == 0 {
		return result, nil
	}
	// 遍历消息库
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		for _, uname := range rooms {
			md5sum := md5.Sum([]byte(uname))
			tbl := "Msg_" + hex.EncodeToString(md5sum[:])
			// 检查表是否存在
			var name string
			err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
			if err != nil {
				continue
			}
			// 计数
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)
			var cnt int64
			if err := db.QueryRowContext(ctx, q).Scan(&cnt); err == nil {
				result[uname] += cnt
			}
		}
	}
	return result, nil
}

// MonthlyTrend 返回每月 sent/received（按 create_time 聚合）
func (ds *DataSource) MonthlyTrend(ctx context.Context, months int) ([]model.MonthlyTrend, error) {
	agg := make(map[string][2]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return []model.MonthlyTrend{}, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
		if err != nil {
			continue
		}
		var tables []string
		for trows.Next() {
			var name string
			if err := trows.Scan(&name); err == nil {
				tables = append(tables, name)
			}
		}
		trows.Close()
		for _, tbl := range tables {
			q := fmt.Sprintf(`SELECT strftime('%%Y-%%m', datetime(create_time, 'unixepoch')) AS ym,
				SUM(CASE WHEN status=2 THEN 1 ELSE 0 END) AS sent,
				SUM(CASE WHEN status!=2 THEN 1 ELSE 0 END) AS recv
				FROM %s GROUP BY ym ORDER BY ym`, tbl)
			rows, err := db.QueryContext(ctx, q)
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
	}
	// 排序输出
	keys := make([]string, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	trends := make([]model.MonthlyTrend, 0, len(keys))
	for _, k := range keys {
		v := agg[k]
		trends = append(trends, model.MonthlyTrend{Date: k, Sent: v[0], Received: v[1]})
	}
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
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
		if err != nil {
			continue
		}
		var tables []string
		for trows.Next() {
			var name string
			if err := trows.Scan(&name); err == nil {
				tables = append(tables, name)
			}
		}
		trows.Close()
		for _, tbl := range tables {
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(create_time,'unixepoch')) AS INTEGER) AS h,
				CAST(strftime('%%w', datetime(create_time,'unixepoch')) AS INTEGER) AS d,
				COUNT(*) FROM %s GROUP BY h,d`, tbl)
			rows, err := db.QueryContext(ctx, q)
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
	}
	return grid, nil
}

// IntimacyBase 统计按联系人（非群聊）聚合的亲密度基础数据（v4）
func (ds *DataSource) IntimacyBase(ctx context.Context) (map[string]*model.IntimacyBase, error) {
	result := make(map[string]*model.IntimacyBase)

	// 构建 md5->username 映射（来自 contact 表）
	md5ToUser := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(Contact); err == nil && cdb != nil {
		rows, err := cdb.QueryContext(ctx, `SELECT username FROM contact`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var uname string
				if rows.Scan(&uname) == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					md5ToUser[hex.EncodeToString(sum[:])] = uname
				}
			}
		}
	}

	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}

	// 列出所有 Msg_% 表并求全局最大时间
	var maxCT int64
	type tbl struct {
		db   *sql.DB
		name string
	}
	var tables []tbl
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
		if err == nil {
			for rows.Next() {
				var name string
				if rows.Scan(&name) == nil {
					tables = append(tables, tbl{db: db, name: name})
				}
			}
			rows.Close()
		}
	}
	for _, t := range tables {
		row := t.db.QueryRowContext(ctx, `SELECT MAX(create_time) FROM `+t.name)
		var v sql.NullInt64
		if row.Scan(&v) == nil && v.Valid && v.Int64 > maxCT {
			maxCT = v.Int64
		}
	}
	if maxCT == 0 {
		return result, nil
	}
	since90 := maxCT - 90*86400
	since7 := maxCT - 7*86400

	// 按表（即按 talker）聚合
	for _, t := range tables {
		// 从表名提取 md5 并映射成 username
		if !strings.HasPrefix(t.name, "Msg_") {
			continue
		}
		md5hex := strings.TrimPrefix(t.name, "Msg_")
		talker := md5ToUser[md5hex]
		if talker == "" {
			continue
		}
		if strings.HasSuffix(talker, "@chatroom") {
			continue
		}

		// total, sent, min, max
		row := t.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(CASE WHEN status=2 THEN 1 ELSE 0 END), MIN(create_time), MAX(create_time) FROM `+t.name)
		var total, sent, minct, maxct sql.NullInt64
		if row.Scan(&total, &sent, &minct, &maxct) == nil {
			base := result[talker]
			if base == nil {
				base = &model.IntimacyBase{UserName: talker}
				result[talker] = base
			}
			if total.Valid {
				base.MsgCount += total.Int64
				base.ReceivedCount += (total.Int64 - func() int64 {
					if sent.Valid {
						return sent.Int64
					}
					return 0
				}())
			}
			if sent.Valid {
				base.SentCount += sent.Int64
			}
			if minct.Valid {
				if base.MinCreateUnix == 0 || minct.Int64 < base.MinCreateUnix {
					base.MinCreateUnix = minct.Int64
				}
			}
			if maxct.Valid {
				if maxct.Int64 > base.MaxCreateUnix {
					base.MaxCreateUnix = maxct.Int64
				}
			}
		}

		// 活跃天数
		row2 := t.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT date(datetime(create_time,'unixepoch'))) FROM `+t.name)
		var days sql.NullInt64
		if row2.Scan(&days) == nil && days.Valid {
			base := result[talker]
			if base == nil {
				base = &model.IntimacyBase{UserName: talker}
				result[talker] = base
			}
			base.MessagingDays += days.Int64
		}

		// 最近90天消息数
		row3 := t.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.name+` WHERE create_time>=?`, since90)
		var c90 sql.NullInt64
		if row3.Scan(&c90) == nil && c90.Valid {
			base := result[talker]
			if base == nil {
				base = &model.IntimacyBase{UserName: talker}
				result[talker] = base
			}
			base.Last90DaysMsg += c90.Int64
		}

		// 过去7天发送
		row4 := t.db.QueryRowContext(ctx, `SELECT SUM(CASE WHEN status=2 THEN 1 ELSE 0 END) FROM `+t.name+` WHERE create_time>=?`, since7)
		var s7 sql.NullInt64
		if row4.Scan(&s7) == nil && s7.Valid {
			base := result[talker]
			if base == nil {
				base = &model.IntimacyBase{UserName: talker}
				result[talker] = base
			}
			base.Past7DaysSentMsg += s7.Int64
		}
	}

	return result, nil
}

func mapV4TypeToLabel(t int64) string {
	// 依据文档统一的消息类型映射
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
		return "XML消息"
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

// GroupTodayMessageCounts 统计群聊今日消息数（v4）：通过 chat_room.username 计算 md5 映射到 Msg_ 表，create_time >= 今日零点
func (ds *DataSource) GroupTodayMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	cdb, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return result, nil
	}
	// 获取所有群聊用户名
	urows, err := cdb.QueryContext(ctx, `SELECT username FROM chat_room`)
	if err != nil {
		return result, nil
	}
	var rooms []string
	for urows.Next() {
		var u string
		if err := urows.Scan(&u); err == nil {
			rooms = append(rooms, u)
		}
	}
	urows.Close()
	if len(rooms) == 0 {
		return result, nil
	}
	// 今日零点
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	since := today.Unix()
	// 遍历消息库
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		for _, uname := range rooms {
			md5sum := md5.Sum([]byte(uname))
			tbl := "Msg_" + hex.EncodeToString(md5sum[:])
			// 检查表是否存在
			var name string
			err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
			if err != nil {
				continue
			}
			// 计数
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE create_time >= ?`, tbl)
			var cnt int64
			if err := db.QueryRowContext(ctx, q, since).Scan(&cnt); err == nil {
				result[uname] += cnt
			}
		}
	}
	return result, nil
}

// GroupTodayHourly 统计群聊今日按小时消息数（v4）
func (ds *DataSource) GroupTodayHourly(ctx context.Context) (map[string][24]int64, error) {
	result := make(map[string][24]int64)
	cdb, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return result, nil
	}
	urows, err := cdb.QueryContext(ctx, `SELECT username FROM chat_room`)
	if err != nil {
		return result, nil
	}
	var rooms []string
	for urows.Next() {
		var u string
		if urows.Scan(&u) == nil {
			rooms = append(rooms, u)
		}
	}
	urows.Close()
	if len(rooms) == 0 {
		return result, nil
	}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	end := start + 86400
	for _, db := range dbs {
		for _, uname := range rooms {
			md5sum := md5.Sum([]byte(uname))
			tbl := "Msg_" + hex.EncodeToString(md5sum[:])
			var name string
			if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); err != nil {
				continue
			}
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(create_time,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM %s WHERE create_time >= ? AND create_time < ? GROUP BY h`, tbl)
			rows, err := db.QueryContext(ctx, q, start, end)
			if err != nil {
				continue
			}
			for rows.Next() {
				var hour int
				var cnt int64
				if rows.Scan(&hour, &cnt) == nil {
					if hour >= 0 && hour < 24 {
						bucket := result[uname]
						bucket[hour] += cnt
						result[uname] = bucket
					}
				}
			}
			rows.Close()
		}
	}
	return result, nil
}

// GroupWeekMessageCount 统计本周(周一00:00起至当前)所有群聊消息总数
// 复用 GroupMessageCounts + 时间过滤会很重，这里直接遍历相关 Msg_ 表做时间范围聚合
func (ds *DataSource) GroupWeekMessageCount(ctx context.Context) (int64, error) {
	var total int64
	cdb, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return 0, nil
	}
	// 群列表
	urows, err := cdb.QueryContext(ctx, `SELECT username FROM chat_room`)
	if err != nil {
		return 0, nil
	}
	var rooms []string
	for urows.Next() {
		var u string
		if urows.Scan(&u) == nil {
			rooms = append(rooms, u)
		}
	}
	urows.Close()
	if len(rooms) == 0 {
		return 0, nil
	}
	now := time.Now()
	// 计算周一 00:00
	wday := int(now.Weekday()) // Sunday=0
	// 以周一为起点，若是周日(0)则回退6天
	offset := wday - 1
	if wday == 0 {
		offset = -6
	}
	monday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -offset)
	since := monday.Unix()
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return 0, nil
	}
	for _, db := range dbs {
		for _, uname := range rooms {
			md5sum := md5.Sum([]byte(uname))
			tbl := "Msg_" + hex.EncodeToString(md5sum[:])
			var name string
			if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); err != nil {
				continue
			}
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE create_time >= ?`, tbl)
			var cnt int64
			if err := db.QueryRowContext(ctx, q, since).Scan(&cnt); err == nil {
				total += cnt
			}
		}
	}
	return total, nil
}

// GlobalTodayHourly 返回今日(本地时区)每小时全部消息量（v4）
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
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Msg_%'`)
		if err != nil {
			continue
		}
		var tables []string
		for trows.Next() {
			var name string
			if trows.Scan(&name) == nil {
				tables = append(tables, name)
			}
		}
		trows.Close()
		for _, tbl := range tables {
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(create_time,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM %s WHERE create_time >= ? AND create_time < ? GROUP BY h`, tbl)
			rows, err := db.QueryContext(ctx, q, start, end)
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
	}
	return hours, nil
}

// GroupMessageTypeStats 统计群聊消息类型分布（v4）
func (ds *DataSource) GroupMessageTypeStats(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	cdb, err := ds.dbm.GetDB(Contact)
	if err != nil {
		return result, nil
	}
	urows, err := cdb.QueryContext(ctx, `SELECT username FROM chat_room`)
	if err != nil {
		return result, nil
	}
	var rooms []string
	for urows.Next() {
		var u string
		if urows.Scan(&u) == nil {
			rooms = append(rooms, u)
		}
	}
	urows.Close()
	if len(rooms) == 0 {
		return result, nil
	}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		for _, uname := range rooms {
			md5sum := md5.Sum([]byte(uname))
			tbl := "Msg_" + hex.EncodeToString(md5sum[:])
			// 先统计非49
			q := fmt.Sprintf(`SELECT local_type, COUNT(*) FROM %s WHERE local_type != 49 GROUP BY local_type`, tbl)
			rows, err := db.QueryContext(ctx, q)
			if err == nil {
				for rows.Next() {
					var t int64
					var cnt int64
					if rows.Scan(&t, &cnt) == nil {
						label := mapV4TypeToLabel(t)
						if label != "" {
							result[label] += cnt
						}
					}
				}
				rows.Close()
			}
			// 处理49
			q49 := fmt.Sprintf(`SELECT message_content FROM %s WHERE local_type=49`, tbl)
			orows, err := db.QueryContext(ctx, q49)
			if err == nil {
				for orows.Next() {
					var mc []byte
					if err := orows.Scan(&mc); err == nil {
						lc := strings.ToLower(string(mc))
						if strings.Contains(lc, "<appmsg") {
							if strings.Contains(lc, "<type>") && strings.Contains(lc, "</type>") {
								i1 := strings.Index(lc, "<type>")
								i2 := strings.Index(lc[i1+6:], "</type>")
								if i1 >= 0 && i2 > 0 {
									val := strings.TrimSpace(lc[i1+6 : i1+6+i2])
									if val == "6" {
										result["文件消息"]++
										continue
									}
									if val == "5" || val == "33" {
										result["链接消息"]++
										continue
									}
								}
							}
						}
						if strings.Contains(lc, "http://") || strings.Contains(lc, "https://") {
							result["链接消息"]++
							continue
						}
						result["XML消息"]++
					}
				}
				orows.Close()
			}
		}
	}
	return result, nil
}
