package darwinv3

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
	Message  = "message"
	Contact  = "contact"
	ChatRoom = "chatroom"
	Session  = "session"
	Media    = "media"
)

var Groups = []*dbm.Group{
	{
		Name:      Message,
		Pattern:   `^msg_([0-9]?[0-9])?\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Contact,
		Pattern:   `^wccontact_new2\.db$`,
		BlackList: []string{},
	},
	{
		Name:      ChatRoom,
		Pattern:   `group_new\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Session,
		Pattern:   `^session_new\.db$`,
		BlackList: []string{},
	},
	{
		Name:      Media,
		Pattern:   `^hldata\.db$`,
		BlackList: []string{},
	},
}

type DataSource struct {
	path string
	dbm  *dbm.DBManager

	talkerDBMap      map[string]string
	user2DisplayName map[string]string

	messageStores      []*msgstore.Store
	messageStoreByPath map[string]*msgstore.Store
	messageStoreMu     sync.RWMutex
}

func New(path string) (*DataSource, error) {
	ds := &DataSource{
		path:               path,
		dbm:                dbm.NewDBManager(path),
		talkerDBMap:        make(map[string]string),
		user2DisplayName:   make(map[string]string),
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
	if err := ds.initChatRoomDb(); err != nil {
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
	ds.dbm.AddCallback(ChatRoom, func(event fsnotify.Event) error {
		if !event.Op.Has(fsnotify.Create) {
			return nil
		}
		if err := ds.initChatRoomDb(); err != nil {
			log.Err(err).Msgf("Failed to reinitialize chatroom DB: %s", event.Name)
		}
		return nil
	})

	return ds, nil
}

func (ds *DataSource) SetCallback(group string, callback func(event fsnotify.Event) error) error {
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
			if store.StartTime.IsZero() || store.EndTime.IsZero() {
				continue
			}
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
			ds.talkerDBMap = make(map[string]string)
			return nil
		}
		return err
	}
	// 处理每个数据库文件
	talkerDBMap := make(map[string]string)
	storeByPath := make(map[string]*msgstore.Store)
	stores := make([]*msgstore.Store, 0, len(dbPaths))
	for _, filePath := range dbPaths {
		db, err := ds.dbm.OpenDB(filePath)
		if err != nil {
			log.Err(err).Msgf("获取数据库 %s 失败", filePath)
			continue
		}

		baseName := filepath.Base(filePath)
		id := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		store := &msgstore.Store{
			ID:        id,
			FilePath:  filePath,
			FileName:  baseName,
			IndexPath: filepath.Join(ds.path, "indexes", "messages", id+".fts.db"),
			Talkers:   make(map[string]struct{}),
		}

		var minTS int64
		var maxTS int64
		haveMin := false
		haveMax := false

		// 获取所有表名
		rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%'")
		if err != nil {
			log.Err(err).Msgf("数据库 %s 中没有 Chat 表", filePath)
		} else {
			for rows.Next() {
				var tableName string
				if err := rows.Scan(&tableName); err != nil {
					log.Err(err).Msgf("数据库 %s 扫描表名失败", filePath)
					continue
				}

				// 从表名中提取可能的talker信息
				talkerMd5 := extractTalkerFromTableName(tableName)
				if talkerMd5 == "" {
					continue
				}
				talkerDBMap[talkerMd5] = filePath
				store.Talkers[talkerMd5] = struct{}{}

				row := db.QueryRow(fmt.Sprintf("SELECT MIN(MsgCreateTime), MAX(MsgCreateTime) FROM %s", tableName))
				var tableMin, tableMax sql.NullInt64
				if err := row.Scan(&tableMin, &tableMax); err != nil {
					log.Debug().Err(err).Msgf("查询 %s 时间范围失败", tableName)
					continue
				}

				if tableMin.Valid {
					if !haveMin || tableMin.Int64 < minTS {
						minTS = tableMin.Int64
					}
					haveMin = true
				}
				if tableMax.Valid {
					if !haveMax || tableMax.Int64 > maxTS {
						maxTS = tableMax.Int64
					}
					haveMax = true
				}
			}
			rows.Close()
		}

		if len(store.Talkers) == 0 {
			store.Talkers = nil
		}
		if haveMin {
			store.StartTime = time.Unix(minTS, 0)
		}
		if haveMax {
			store.EndTime = time.Unix(maxTS, 0).Add(time.Second)
		}

		stores = append(stores, store)
		storeByPath[filePath] = store
	}

	ds.talkerDBMap = talkerDBMap
	ds.messageStoreMu.Lock()
	ds.messageStores = stores
	ds.messageStoreByPath = storeByPath
	ds.messageStoreMu.Unlock()
	return nil
}

// IntimacyBase 统计按联系人（非群聊）聚合的亲密度基础数据（darwin v3）
func (ds *DataSource) IntimacyBase(ctx context.Context) (map[string]*model.IntimacyBase, error) {
	result := make(map[string]*model.IntimacyBase)

	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}

	var maxCT int64
	type tbl struct {
		db   *sql.DB
		name string
	}
	var tables []tbl
	for _, db := range dbs {
		rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%'`)
		if err == nil {
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err == nil {
					tables = append(tables, tbl{db: db, name: name})
				}
			}
			rows.Close()
		}
	}
	for _, t := range tables {
		row := t.db.QueryRowContext(ctx, `SELECT MAX(msgCreateTime) FROM `+t.name)
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

	for _, t := range tables {
		// 基础统计：total, sent, recv, min, max；darwin v3: mesDes: 1=接收? 2=发送? 需按现有实现参考：在其它地方通常 IsSender=1 v3 windows；v3 darwin字段不同，这里按 status 字段近似处理：当 mesDes=2 视为发送
		q := `SELECT COALESCE(n.user_name,''),
			COUNT(*) AS msg_count,
			MIN(m.msgCreateTime) AS minct,
			MAX(m.msgCreateTime) AS maxct,
			SUM(CASE WHEN m.mesDes=0 THEN 1 ELSE 0 END) AS sent,
			SUM(CASE WHEN m.mesDes!=0 THEN 1 ELSE 0 END) AS recv
			FROM ` + t.name + ` m
			LEFT JOIN Name2Id n ON m.realChatUsrNameId = n.rowid
			WHERE (n.user_name IS NULL OR n.user_name NOT LIKE '%@chatroom')
			GROUP BY COALESCE(n.user_name,'')`
		rows, err := t.db.QueryContext(ctx, q)
		if err == nil {
			for rows.Next() {
				var uname string
				var cnt, minct, maxct, sent, recv int64
				if err := rows.Scan(&uname, &cnt, &minct, &maxct, &sent, &recv); err == nil {
					if strings.TrimSpace(uname) == "" {
						continue
					}
					base := result[uname]
					if base == nil {
						base = &model.IntimacyBase{UserName: uname}
						result[uname] = base
					}
					base.MsgCount += cnt
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

		// 活跃天数
		q2 := `SELECT COALESCE(n.user_name,''), COUNT(DISTINCT date(datetime(m.msgCreateTime,'unixepoch'))) AS days
			FROM ` + t.name + ` m LEFT JOIN Name2Id n ON m.realChatUsrNameId = n.rowid
			WHERE (n.user_name IS NULL OR n.user_name NOT LIKE '%@chatroom')
			GROUP BY COALESCE(n.user_name,'')`
		rows2, err := t.db.QueryContext(ctx, q2)
		if err == nil {
			for rows2.Next() {
				var uname string
				var days int64
				if err := rows2.Scan(&uname, &days); err == nil {
					if strings.TrimSpace(uname) == "" {
						continue
					}
					base := result[uname]
					if base == nil {
						base = &model.IntimacyBase{UserName: uname}
						result[uname] = base
					}
					base.MessagingDays += days
				}
			}
			rows2.Close()
		}

		// 最近90天消息
		q3 := `SELECT COALESCE(n.user_name,''), COUNT(*)
			FROM ` + t.name + ` m LEFT JOIN Name2Id n ON m.realChatUsrNameId = n.rowid
			WHERE m.msgCreateTime>=? AND (n.user_name IS NULL OR n.user_name NOT LIKE '%@chatroom')
			GROUP BY COALESCE(n.user_name,'')`
		rows3, err := t.db.QueryContext(ctx, q3, since90)
		if err == nil {
			for rows3.Next() {
				var uname string
				var cnt int64
				if err := rows3.Scan(&uname, &cnt); err == nil {
					if strings.TrimSpace(uname) == "" {
						continue
					}
					base := result[uname]
					if base == nil {
						base = &model.IntimacyBase{UserName: uname}
						result[uname] = base
					}
					base.Last90DaysMsg += cnt
				}
			}
			rows3.Close()
		}

		// 过去7天发送
		q4 := `SELECT COALESCE(n.user_name,''), SUM(CASE WHEN m.mesDes=0 THEN 1 ELSE 0 END)
			FROM ` + t.name + ` m LEFT JOIN Name2Id n ON m.realChatUsrNameId = n.rowid
			WHERE m.msgCreateTime>=? AND (n.user_name IS NULL OR n.user_name NOT LIKE '%@chatroom')
			GROUP BY COALESCE(n.user_name,'')`
		rows4, err := t.db.QueryContext(ctx, q4, since7)
		if err == nil {
			for rows4.Next() {
				var uname string
				var cnt sql.NullInt64
				if err := rows4.Scan(&uname, &cnt); err == nil {
					if strings.TrimSpace(uname) == "" {
						continue
					}
					base := result[uname]
					if base == nil {
						base = &model.IntimacyBase{UserName: uname}
						result[uname] = base
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

func (ds *DataSource) initChatRoomDb() error {
	db, err := ds.dbm.GetDB(ChatRoom)
	if err != nil {
		if strings.Contains(err.Error(), "db file not found") {
			ds.user2DisplayName = make(map[string]string)
			return nil
		}
		return err
	}

	rows, err := db.Query("SELECT m_nsUsrName, IFNULL(nickname,\"\") FROM GroupMember")
	if err != nil {
		log.Err(err).Msg("获取群聊成员失败")
		return nil
	}

	user2DisplayName := make(map[string]string)
	for rows.Next() {
		var user string
		var nickName string
		if err := rows.Scan(&user, &nickName); err != nil {
			log.Err(err).Msg("扫描表名失败")
			continue
		}
		user2DisplayName[user] = nickName
	}
	rows.Close()
	ds.user2DisplayName = user2DisplayName

	return nil
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

	// 对每个talker进行查询
	for _, talkerItem := range talkers {
		// 检查上下文是否已取消
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// 在 darwinv3 中，需要先找到对应的数据库
		_talkerMd5Bytes := md5.Sum([]byte(talkerItem))
		talkerMd5 := hex.EncodeToString(_talkerMd5Bytes[:])
		dbPath, ok := ds.talkerDBMap[talkerMd5]
		if !ok {
			// 如果找不到对应的数据库，跳过此talker
			continue
		}

		db, err := ds.dbm.OpenDB(dbPath)
		if err != nil {
			log.Error().Msgf("数据库 %s 未打开", dbPath)
			continue
		}

		tableName := fmt.Sprintf("Chat_%s", talkerMd5)

		// 构建查询条件
		query := fmt.Sprintf(`
			SELECT msgCreateTime, msgContent, messageType, mesDes
			FROM %s 
			WHERE msgCreateTime >= ? AND msgCreateTime <= ? 
			ORDER BY msgCreateTime ASC
		`, tableName)

		// 执行查询
		rows, err := db.QueryContext(ctx, query, startTime.Unix(), endTime.Unix())
		if err != nil {
			// 如果表不存在，跳过此talker
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			log.Err(err).Msgf("从数据库 %s 查询消息失败", dbPath)
			continue
		}

		// 处理查询结果，在读取时进行过滤
		for rows.Next() {
			var msg model.MessageDarwinV3
			err := rows.Scan(
				&msg.MsgCreateTime,
				&msg.MsgContent,
				&msg.MessageType,
				&msg.MesDes,
			)
			if err != nil {
				rows.Close()
				log.Err(err).Msgf("扫描消息行失败")
				continue
			}

			// 将消息包装为通用模型
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

	// 对所有消息按时间排序
	// FIXME 不同 talker 需要使用 Time 排序
	sort.Slice(filteredMessages, func(i, j int) bool {
		return filteredMessages[i].Time.Before(filteredMessages[j].Time)
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

// 从表名中提取 talker
func extractTalkerFromTableName(tableName string) string {

	if !strings.HasPrefix(tableName, "Chat_") {
		return ""
	}

	if strings.HasSuffix(tableName, "_dels") {
		return ""
	}

	return strings.TrimPrefix(tableName, "Chat_")
}

// GetContacts 实现获取联系人信息的方法
func (ds *DataSource) GetContacts(ctx context.Context, key string, limit, offset int) ([]*model.Contact, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT IFNULL(m_nsUsrName,""), IFNULL(nickname,""), IFNULL(m_nsRemark,""), m_uiSex, IFNULL(m_nsAliasName,"") 
				FROM WCContact 
				WHERE m_nsUsrName = ? OR nickname = ? OR m_nsRemark = ? OR m_nsAliasName = ?`
		args = []interface{}{key, key, key, key}
	} else {
		// 查询所有联系人
		query = `SELECT IFNULL(m_nsUsrName,""), IFNULL(nickname,""), IFNULL(m_nsRemark,""), m_uiSex, IFNULL(m_nsAliasName,"") 
				FROM WCContact`
	}

	// 添加排序、分页
	query += ` ORDER BY m_nsUsrName`
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
		var contactDarwinV3 model.ContactDarwinV3
		err := rows.Scan(
			&contactDarwinV3.M_nsUsrName,
			&contactDarwinV3.Nickname,
			&contactDarwinV3.M_nsRemark,
			&contactDarwinV3.M_uiSex,
			&contactDarwinV3.M_nsAliasName,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		contacts = append(contacts, contactDarwinV3.Wrap())
	}

	return contacts, nil
}

// GetChatRooms 实现获取群聊信息的方法
func (ds *DataSource) GetChatRooms(ctx context.Context, key string, limit, offset int) ([]*model.ChatRoom, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT IFNULL(m_nsUsrName,""), IFNULL(nickname,""), IFNULL(m_nsRemark,""), IFNULL(m_nsChatRoomMemList,""), IFNULL(m_nsChatRoomAdminList,"") 
				FROM GroupContact 
				WHERE m_nsUsrName = ? OR nickname = ? OR m_nsRemark = ?`
		args = []interface{}{key, key, key}
	} else {
		// 查询所有群聊
		query = `SELECT IFNULL(m_nsUsrName,""), IFNULL(nickname,""), IFNULL(m_nsRemark,""), IFNULL(m_nsChatRoomMemList,""), IFNULL(m_nsChatRoomAdminList,"") 
				FROM GroupContact`
	}

	// 添加排序、分页
	query += ` ORDER BY m_nsUsrName`
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	// 执行查询
	db, err := ds.dbm.GetDB(ChatRoom)
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
		var chatRoomDarwinV3 model.ChatRoomDarwinV3
		err := rows.Scan(
			&chatRoomDarwinV3.M_nsUsrName,
			&chatRoomDarwinV3.Nickname,
			&chatRoomDarwinV3.M_nsRemark,
			&chatRoomDarwinV3.M_nsChatRoomMemList,
			&chatRoomDarwinV3.M_nsChatRoomAdminList,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		chatRooms = append(chatRooms, chatRoomDarwinV3.Wrap(ds.user2DisplayName))
	}

	// 如果没有找到群聊，尝试通过联系人查找
	if len(chatRooms) == 0 && key != "" {
		contacts, err := ds.GetContacts(ctx, key, 1, 0)
		if err == nil && len(contacts) > 0 && strings.HasSuffix(contacts[0].UserName, "@chatroom") {
			// 再次尝试通过用户名查找群聊
			rows, err := db.QueryContext(ctx,
				`SELECT IFNULL(m_nsUsrName,""), IFNULL(nickname,""), IFNULL(m_nsRemark,""), IFNULL(m_nsChatRoomMemList,""), IFNULL(m_nsChatRoomAdminList,"") 
				FROM GroupContact 
				WHERE m_nsUsrName = ?`,
				contacts[0].UserName)

			if err != nil {
				return nil, errors.QueryFailed(query, err)
			}
			defer rows.Close()

			for rows.Next() {
				var chatRoomDarwinV3 model.ChatRoomDarwinV3
				err := rows.Scan(
					&chatRoomDarwinV3.M_nsUsrName,
					&chatRoomDarwinV3.Nickname,
					&chatRoomDarwinV3.M_nsRemark,
					&chatRoomDarwinV3.M_nsChatRoomMemList,
					&chatRoomDarwinV3.M_nsChatRoomAdminList,
				)

				if err != nil {
					return nil, errors.ScanRowFailed(err)
				}

				chatRooms = append(chatRooms, chatRoomDarwinV3.Wrap(ds.user2DisplayName))
			}

			// 如果群聊记录不存在，但联系人记录存在，创建一个模拟的群聊对象
			if len(chatRooms) == 0 {
				chatRooms = append(chatRooms, &model.ChatRoom{
					Name:  contacts[0].UserName,
					Users: make([]model.ChatRoomUser, 0),
				})
			}
		}
	}

	return chatRooms, nil
}

// GetSessions 实现获取会话信息的方法
func (ds *DataSource) GetSessions(ctx context.Context, key string, limit, offset int) ([]*model.Session, error) {
	var query string
	var args []interface{}

	if key != "" {
		// 按照关键字查询
		query = `SELECT m_nsUserName, m_uLastTime 
				FROM SessionAbstract 
				WHERE m_nsUserName = ?`
		args = []interface{}{key}
	} else {
		// 查询所有会话
		query = `SELECT m_nsUserName, m_uLastTime 
				FROM SessionAbstract`
	}

	// 添加排序、分页
	query += ` ORDER BY m_uLastTime DESC`
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
		var sessionDarwinV3 model.SessionDarwinV3
		err := rows.Scan(
			&sessionDarwinV3.M_nsUserName,
			&sessionDarwinV3.M_uLastTime,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		// 包装成通用模型
		session := sessionDarwinV3.Wrap()

		// 尝试获取联系人信息以补充会话信息
		contacts, err := ds.GetContacts(ctx, session.UserName, 1, 0)
		if err == nil && len(contacts) > 0 {
			session.NickName = contacts[0].DisplayName()
		} else {
			// 尝试获取群聊信息
			chatRooms, err := ds.GetChatRooms(ctx, session.UserName, 1, 0)
			if err == nil && len(chatRooms) > 0 {
				session.NickName = chatRooms[0].DisplayName()
			}
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

func (ds *DataSource) GetMedia(ctx context.Context, _type string, key string) (*model.Media, error) {
	if key == "" {
		return nil, errors.ErrKeyEmpty
	}
	query := `SELECT 
    r.mediaMd5,
    r.mediaSize,
    r.inodeNumber,
    r.modifyTime,
    d.relativePath,
    d.fileName
FROM 
    HlinkMediaRecord r
JOIN 
    HlinkMediaDetail d ON r.inodeNumber = d.inodeNumber
WHERE 
    r.mediaMd5 = ?`
	args := []interface{}{key}
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
		var mediaDarwinV3 model.MediaDarwinV3
		err := rows.Scan(
			&mediaDarwinV3.MediaMd5,
			&mediaDarwinV3.MediaSize,
			&mediaDarwinV3.InodeNumber,
			&mediaDarwinV3.ModifyTime,
			&mediaDarwinV3.RelativePath,
			&mediaDarwinV3.FileName,
		)

		if err != nil {
			return nil, errors.ScanRowFailed(err)
		}

		// 包装成通用模型
		media = mediaDarwinV3.Wrap()
	}

	if media == nil {
		return nil, errors.ErrMediaNotFound
	}

	return media, nil
}

// Close 实现关闭数据库连接的方法
func (ds *DataSource) Close() error {
	return ds.dbm.Close()
}

func (ds *DataSource) GetDatasetFingerprint(context.Context) (string, error) {
	return ds.dbm.FingerprintForGroups(Message)
}

// GetAvatar returns not found for darwin v3 (no head image source known here)
func (ds *DataSource) GetAvatar(ctx context.Context, username string, size string) (*model.Avatar, error) {
	return nil, errors.ErrAvatarNotFound
}

// GlobalMessageStats 聚合统计（Darwin v3）
func (ds *DataSource) GlobalMessageStats(ctx context.Context) (*model.GlobalMessageStats, error) {
	stats := &model.GlobalMessageStats{ByType: make(map[string]int64)}
	// 遍历所有消息库，枚举 Chat_% 表
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return stats, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
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
			q := fmt.Sprintf(`SELECT COUNT(*) AS total,
				SUM(CASE WHEN mesDes=0 THEN 1 ELSE 0 END) AS sent,
				MIN(msgCreateTime) AS minct,
				MAX(msgCreateTime) AS maxct FROM %s`, tbl)
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
			// by type（先统计非 49）
			q2 := fmt.Sprintf(`SELECT messageType, COUNT(*) FROM %s WHERE messageType != 49 GROUP BY messageType`, tbl)
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
			// 细分 49
			q49 := fmt.Sprintf(`SELECT msgContent FROM %s WHERE messageType = 49`, tbl)
			orows, err := db.QueryContext(ctx, q49)
			if err == nil {
				for orows.Next() {
					var mc string
					if err := orows.Scan(&mc); err == nil {
						lc := strings.ToLower(mc)
						if strings.Contains(lc, "<appmsg") {
							if strings.Contains(lc, "<type>") && strings.Contains(lc, "</type>") {
								i1 := strings.Index(lc, "<type>")
								i2 := strings.Index(lc[i1+6:], "</type>")
								if i1 >= 0 && i2 > 0 {
									val := lc[i1+6 : i1+6+i2]
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
						}
						if strings.Contains(lc, "http://") || strings.Contains(lc, "https://") {
							stats.ByType["链接消息"]++
							continue
						}
						stats.ByType["XML消息"]++
					}
				}
				orows.Close()
			}
		}
	}
	return stats, nil
}

// GroupMessageCounts 统计群聊消息数量（Darwin v3）
func (ds *DataSource) GroupMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	// 先获取所有群聊用户名，构建 md5 -> username 映射
	mapping := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(ChatRoom); err == nil {
		rows, err := cdb.QueryContext(ctx, `SELECT IFNULL(m_nsUsrName,"") FROM GroupContact`)
		if err == nil {
			for rows.Next() {
				var uname string
				if err := rows.Scan(&uname); err == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					mapping["Chat_"+hex.EncodeToString(sum[:])] = uname
				}
			}
			rows.Close()
		}
	}
	// 遍历消息库，按表计数并映射回 username
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
		if err != nil {
			continue
		}
		for trows.Next() {
			var tbl string
			if err := trows.Scan(&tbl); err != nil {
				continue
			}
			var cnt int64
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, tbl)
			if err := db.QueryRowContext(ctx, q).Scan(&cnt); err == nil {
				key := tbl
				if uname, ok := mapping[tbl]; ok {
					key = uname
				}
				result[key] += cnt
			}
		}
		trows.Close()
	}
	return result, nil
}

// GroupTodayMessageCounts 统计群聊今日消息数（Darwin v3）：Chat_% 表按 msgCreateTime 过滤 >= 今日零点
func (ds *DataSource) GroupTodayMessageCounts(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	// 构建 md5->username 映射（来自 GroupContact）
	mapping := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(ChatRoom); err == nil {
		rows, err := cdb.QueryContext(ctx, `SELECT IFNULL(m_nsUsrName,"") FROM GroupContact`)
		if err == nil {
			for rows.Next() {
				var uname string
				if err := rows.Scan(&uname); err == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					mapping["Chat_"+hex.EncodeToString(sum[:])] = uname
				}
			}
			rows.Close()
		}
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
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
		if err != nil {
			continue
		}
		for trows.Next() {
			var tbl string
			if err := trows.Scan(&tbl); err != nil {
				continue
			}
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE msgCreateTime >= ?`, tbl)
			var cnt int64
			if err := db.QueryRowContext(ctx, q, since).Scan(&cnt); err == nil {
				key := tbl
				if uname, ok := mapping[tbl]; ok {
					key = uname
				}
				result[key] += cnt
			}
		}
		trows.Close()
	}
	return result, nil
}

// GroupTodayHourly 统计群聊今日按小时消息数（Darwin v3）
func (ds *DataSource) GroupTodayHourly(ctx context.Context) (map[string][24]int64, error) {
	result := make(map[string][24]int64)
	mapping := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(ChatRoom); err == nil {
		rows, err := cdb.QueryContext(ctx, `SELECT IFNULL(m_nsUsrName,"" ) FROM GroupContact`)
		if err == nil {
			for rows.Next() {
				var uname string
				if rows.Scan(&uname) == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					mapping["Chat_"+hex.EncodeToString(sum[:])] = uname
				}
			}
			rows.Close()
		}
	}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	end := start + 86400
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
		if err != nil {
			continue
		}
		for trows.Next() {
			var tbl string
			if trows.Scan(&tbl) != nil {
				continue
			}
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(msgCreateTime,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM %s WHERE msgCreateTime >= ? AND msgCreateTime < ? GROUP BY h`, tbl)
			rows, err := db.QueryContext(ctx, q, start, end)
			if err != nil {
				continue
			}
			key := tbl
			if uname, ok := mapping[tbl]; ok {
				key = uname
			}
			for rows.Next() {
				var hour int
				var cnt int64
				if rows.Scan(&hour, &cnt) == nil {
					if hour >= 0 && hour < 24 {
						bucket := result[key]
						bucket[hour] += cnt
						result[key] = bucket
					}
				}
			}
			rows.Close()
		}
		trows.Close()
	}
	return result, nil
}

// GroupWeekMessageCount 统计本周(周一00:00起)所有群聊消息总数（Darwin v3）
func (ds *DataSource) GroupWeekMessageCount(ctx context.Context) (int64, error) {
	var total int64
	// 构建映射便于遍历表名
	mapping := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(ChatRoom); err == nil {
		rows, err := cdb.QueryContext(ctx, `SELECT IFNULL(m_nsUsrName,"") FROM GroupContact`)
		if err == nil {
			for rows.Next() {
				var uname string
				if rows.Scan(&uname) == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					mapping["Chat_"+hex.EncodeToString(sum[:])] = uname
				}
			}
			rows.Close()
		}
	}
	now := time.Now()
	wday := int(now.Weekday())
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
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
		if err != nil {
			continue
		}
		for trows.Next() {
			var tbl string
			if trows.Scan(&tbl) != nil {
				continue
			}
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE msgCreateTime >= ?`, tbl)
			var cnt int64
			if db.QueryRowContext(ctx, q, since).Scan(&cnt) == nil {
				total += cnt
			}
		}
		trows.Close()
	}
	return total, nil
}

// MonthlyTrend 返回每月 sent/received
func (ds *DataSource) MonthlyTrend(ctx context.Context, months int) ([]model.MonthlyTrend, error) {
	agg := make(map[string][2]int64)
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return []model.MonthlyTrend{}, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
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
			q := fmt.Sprintf(`SELECT strftime('%%Y-%%m', datetime(msgCreateTime, 'unixepoch')) AS ym,
				SUM(CASE WHEN mesDes=0 THEN 1 ELSE 0 END) AS sent,
				SUM(CASE WHEN mesDes!=0 THEN 1 ELSE 0 END) AS recv
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

// GroupMessageTypeStats 统计群聊消息类型分布（Darwin v3）
func (ds *DataSource) GroupMessageTypeStats(ctx context.Context) (map[string]int64, error) {
	result := make(map[string]int64)
	// 构建群聊 md5 映射
	mapping := make(map[string]string)
	if cdb, err := ds.dbm.GetDB(ChatRoom); err == nil {
		if rows, err2 := cdb.QueryContext(ctx, `SELECT IFNULL(m_nsUsrName,"") FROM GroupContact`); err2 == nil {
			for rows.Next() {
				var uname string
				if rows.Scan(&uname) == nil && uname != "" {
					sum := md5.Sum([]byte(uname))
					mapping["Chat_"+hex.EncodeToString(sum[:])] = uname
				}
			}
			rows.Close()
		}
	}
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return result, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
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
			if _, ok := mapping[tbl]; !ok {
				continue
			}
			// 非49
			q := fmt.Sprintf(`SELECT messageType, COUNT(*) FROM %s WHERE messageType != 49 GROUP BY messageType`, tbl)
			rows, err2 := db.QueryContext(ctx, q)
			if err2 == nil {
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
			// 49
			q49 := fmt.Sprintf(`SELECT msgContent FROM %s WHERE messageType = 49`, tbl)
			orows, err3 := db.QueryContext(ctx, q49)
			if err3 == nil {
				for orows.Next() {
					var mc string
					if orows.Scan(&mc) == nil {
						lc := strings.ToLower(mc)
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

// Heatmap 小时x星期（wday: 0=Sunday..6）
func (ds *DataSource) Heatmap(ctx context.Context) ([24][7]int64, error) {
	var grid [24][7]int64
	dbs, err := ds.dbm.GetDBs(Message)
	if err != nil {
		return grid, nil
	}
	for _, db := range dbs {
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
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
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(msgCreateTime,'unixepoch')) AS INTEGER) AS h,
				CAST(strftime('%%w', datetime(msgCreateTime,'unixepoch')) AS INTEGER) AS d,
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

// GlobalTodayHourly 返回今日(本地时区)每小时全部消息量（Darwin v3）
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
		trows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'Chat_%' AND name NOT LIKE '%_dels'`)
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
			q := fmt.Sprintf(`SELECT CAST(strftime('%%H', datetime(msgCreateTime,'unixepoch')) AS INTEGER) AS h, COUNT(*) FROM %s WHERE msgCreateTime >= ? AND msgCreateTime < ? GROUP BY h`, tbl)
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

// 本地定义简易类型映射，复用与 v4 类似的类别
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
