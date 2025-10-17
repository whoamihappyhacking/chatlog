package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"

	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/internal/wechatdb/msgstore"
)

const (
	runtimeIndexVersion = "3"
)

var (
	errIndexNotInitialized = errors.New("fts index not initialized")
)

type metadata struct {
	Version     string `json:"version"`
	Fingerprint string `json:"fingerprint"`
	LastBuilt   int64  `json:"last_built"`
}

type storeIndex struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
}

// Index coordinates a set of per-store SQLite FTS indices.
type Index struct {
	mu       sync.RWMutex
	basePath string
	metaPath string
	meta     metadata
	stores   map[string]*storeIndex
}

// Open prepares an Index rooted at basePath.
func Open(basePath string) (*Index, error) {
	if err := os.MkdirAll(basePath, 0o755); err != nil {
		return nil, fmt.Errorf("create index base dir: %w", err)
	}

	metaPath := filepath.Join(basePath, "index-meta.json")
	meta, err := loadMetadata(metaPath)
	if err != nil {
		return nil, fmt.Errorf("load index metadata: %w", err)
	}

	return &Index{
		basePath: basePath,
		metaPath: metaPath,
		meta:     meta,
		stores:   make(map[string]*storeIndex),
	}, nil
}

// Close releases all opened store indices.
func (i *Index) Close() error {
	if i == nil {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	var firstErr error
	for id, si := range i.stores {
		if err := si.close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(i.stores, id)
	}
	return firstErr
}

// Reset removes all materialised indices.
func (i *Index) Reset() error {
	if i == nil {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	for id, si := range i.stores {
		_ = si.close()
		_ = os.Remove(si.path)
		delete(i.stores, id)
	}
	return nil
}

// SyncStores hydrates store handles for the provided message stores.
func (i *Index) SyncStores(stores []*msgstore.Store) error {
	if i == nil {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	desired := make(map[string]*storeIndex, len(stores))
	for _, store := range stores {
		if store == nil {
			continue
		}
		id := strings.TrimSpace(store.ID)
		if id == "" {
			continue
		}
		si, err := i.ensureStoreIndexLocked(store)
		if err != nil {
			return err
		}
		desired[id] = si
	}

	for id, si := range i.stores {
		if _, ok := desired[id]; !ok {
			_ = si.close()
		}
	}

	i.stores = desired
	return nil
}

// EnsureVersion guarantees the on-disk metadata matches the runtime version.
func (i *Index) EnsureVersion() (bool, error) {
	if i == nil {
		return false, nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.meta.Version == runtimeIndexVersion {
		return true, nil
	}

	i.meta.Version = runtimeIndexVersion
	if err := i.saveMetadataLocked(); err != nil {
		return false, err
	}
	return false, nil
}

// UpdateFingerprint stores the provided dataset fingerprint.
func (i *Index) UpdateFingerprint(fp string) error {
	if i == nil {
		return nil
	}

	fp = strings.TrimSpace(fp)

	i.mu.Lock()
	defer i.mu.Unlock()

	i.meta.Fingerprint = fp
	return i.saveMetadataLocked()
}

// Fingerprint returns the stored fingerprint value.
func (i *Index) Fingerprint() string {
	if i == nil {
		return ""
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	return i.meta.Fingerprint
}

// FingerprintMatches compares the stored fingerprint with fp.
func (i *Index) FingerprintMatches(fp string) bool {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return false
	}
	return i.Fingerprint() == fp
}

// EnsureFingerprint updates the stored fingerprint if it differs.
func (i *Index) EnsureFingerprint(fp string) (bool, error) {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		return false, nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.meta.Fingerprint == fp {
		return true, nil
	}

	i.meta.Fingerprint = fp
	if err := i.saveMetadataLocked(); err != nil {
		return false, err
	}
	return false, nil
}

// UpdateLastBuilt records the timestamp of a successful rebuild.
func (i *Index) UpdateLastBuilt(t time.Time) error {
	if i == nil {
		return nil
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	i.meta.LastBuilt = t.Unix()
	return i.saveMetadataLocked()
}

// LastBuilt returns the recorded rebuild timestamp.
func (i *Index) LastBuilt() time.Time {
	if i == nil {
		return time.Time{}
	}

	i.mu.RLock()
	defer i.mu.RUnlock()

	if i.meta.LastBuilt == 0 {
		return time.Time{}
	}
	return time.Unix(i.meta.LastBuilt, 0)
}

// IndexStoreMessages indexes a batch of chat messages for a given store.
func (i *Index) IndexStoreMessages(store *msgstore.Store, messages []*model.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if store == nil {
		return errors.New("nil message store")
	}

	si, err := i.ensureStoreIndex(store)
	if err != nil {
		return err
	}
	return si.indexMessages(messages)
}

// Search performs a federated search across all store indices.
func (i *Index) Search(req *model.SearchRequest, talkers []string, senders []string, startUnix, endUnix int64, offset, limit int) ([]*SearchHit, int, error) {
	if req == nil {
		return nil, 0, errors.New("search request is nil")
	}

	match, err := buildFTSQuery(req.Query)
	if err != nil {
		return nil, 0, err
	}
	if match == "" {
		return []*SearchHit{}, 0, nil
	}

	talkers = dedupeStrings(talkers)
	senders = dedupeStrings(senders)

	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	i.mu.RLock()
	stores := make([]*storeIndex, 0, len(i.stores))
	for _, si := range i.stores {
		stores = append(stores, si)
	}
	i.mu.RUnlock()

	if len(stores) == 0 {
		return []*SearchHit{}, 0, nil
	}

	perStoreLimit := offset + limit
	if perStoreLimit <= 0 {
		perStoreLimit = limit
	}

	combined := make([]*SearchHit, 0, len(stores)*limit)
	total := 0
	for _, si := range stores {
		hits, count, err := si.search(match, talkers, senders, startUnix, endUnix, 0, perStoreLimit)
		if err != nil {
			return nil, 0, err
		}
		total += count
		combined = append(combined, hits...)
	}

	if len(combined) == 0 {
		return []*SearchHit{}, total, nil
	}

	sort.Slice(combined, func(a, b int) bool {
		ha := combined[a]
		hb := combined[b]
		if ha == nil || hb == nil {
			return ha != nil
		}
		if ha.Score != hb.Score {
			return ha.Score < hb.Score
		}
		ta := ha.Message.Time.Unix()
		tb := hb.Message.Time.Unix()
		if ta != tb {
			return ta > tb
		}
		return ha.Message.Seq > hb.Message.Seq
	})

	if offset >= len(combined) {
		return []*SearchHit{}, total, nil
	}

	end := offset + limit
	if end > len(combined) {
		end = len(combined)
	}

	return combined[offset:end], total, nil
}

func (i *Index) ensureStoreIndex(store *msgstore.Store) (*storeIndex, error) {
	if i == nil {
		return nil, errors.New("index is nil")
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	return i.ensureStoreIndexLocked(store)
}

func (i *Index) ensureStoreIndexLocked(store *msgstore.Store) (*storeIndex, error) {
	if store == nil {
		return nil, errors.New("nil message store")
	}

	id := strings.TrimSpace(store.ID)
	if id == "" {
		return nil, errors.New("empty store id")
	}

	path := i.resolveStorePath(store)
	if existing, ok := i.stores[id]; ok {
		if existing.path == path {
			return existing, nil
		}
		_ = existing.close()
	}

	si, err := newStoreIndex(path)
	if err != nil {
		return nil, err
	}

	i.stores[id] = si
	return si, nil
}

func (i *Index) resolveStorePath(store *msgstore.Store) string {
	if store != nil && strings.TrimSpace(store.IndexPath) != "" {
		if filepath.IsAbs(store.IndexPath) {
			return store.IndexPath
		}
		return filepath.Join(i.basePath, store.IndexPath)
	}

	id := strings.TrimSpace(store.ID)
	if id == "" {
		id = fmt.Sprintf("store_%d", time.Now().UnixNano())
	}
	return filepath.Join(i.basePath, id+".fts.db")
}

func newStoreIndex(path string) (*storeIndex, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create store index dir: %w", err)
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal=WAL&_synchronous=NORMAL", filepath.ToSlash(path))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store index: %w", err)
	}

	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &storeIndex{db: db, path: path}, nil
}

func (s *storeIndex) close() error {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func initSchema(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA temp_store = MEMORY;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("init schema (%s): %w", pragma, err)
		}
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS metadata (
key   TEXT PRIMARY KEY,
value TEXT NOT NULL
);`,
		`CREATE TABLE IF NOT EXISTS messages (
doc_id       TEXT NOT NULL UNIQUE,
talker       TEXT NOT NULL,
sender       TEXT NOT NULL,
unix         INTEGER NOT NULL,
seq          INTEGER NOT NULL,
content      TEXT NOT NULL,
message_json TEXT NOT NULL
);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_talker ON messages(talker);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sender ON messages(sender);`,
		`CREATE INDEX IF NOT EXISTS idx_messages_unix ON messages(unix);`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
talker   TEXT PRIMARY KEY,
last_seq INTEGER NOT NULL
);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
content,
content='messages',
content_rowid='rowid',
tokenize='unicode61 remove_diacritics 2'
);`,
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;`,
		`CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;`,
	}

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("init schema statement failed: %w", err)
		}
	}

	return nil
}

func (s *storeIndex) indexMessages(messages []*model.Message) error {
	if len(messages) == 0 {
		return nil
	}

	docs := make([]*document, 0, len(messages))
	maxSeq := make(map[string]int64)

	for _, msg := range messages {
		if msg == nil {
			continue
		}
		doc, err := newDocument(msg)
		if err != nil {
			return err
		}
		docs = append(docs, doc)
		if prev, ok := maxSeq[doc.Talker]; !ok || doc.Seq > prev {
			maxSeq[doc.Talker] = doc.Seq
		}
	}

	if len(docs) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errIndexNotInitialized
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	insertStmt, err := tx.Prepare(`
INSERT INTO messages (doc_id, talker, sender, unix, seq, content, message_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(doc_id) DO UPDATE SET
talker = excluded.talker,
sender = excluded.sender,
unix = excluded.unix,
seq = excluded.seq,
content = excluded.content,
message_json = excluded.message_json
`)
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	for _, doc := range docs {
		if _, err = insertStmt.Exec(doc.ID, doc.Talker, doc.Sender, doc.Unix, doc.Seq, doc.Content, doc.MessageJSON); err != nil {
			return fmt.Errorf("insert message %s: %w", doc.ID, err)
		}
	}

	checkpointStmt, err := tx.Prepare(`
INSERT INTO checkpoints (talker, last_seq)
VALUES (?, ?)
ON CONFLICT(talker) DO UPDATE SET last_seq = CASE WHEN excluded.last_seq > last_seq THEN excluded.last_seq ELSE last_seq END
`)
	if err != nil {
		return err
	}
	defer checkpointStmt.Close()

	for talker, seq := range maxSeq {
		if _, err = checkpointStmt.Exec(talker, seq); err != nil {
			return fmt.Errorf("update checkpoint %s: %w", talker, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (s *storeIndex) search(match string, talkers []string, senders []string, startUnix, endUnix int64, offset, limit int) ([]*SearchHit, int, error) {
	if s == nil {
		return nil, 0, errIndexNotInitialized
	}

	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()
	if db == nil {
		return nil, 0, errIndexNotInitialized
	}

	whereClauses := []string{}
	args := []interface{}{match}

	if len(talkers) > 0 {
		placeholders := strings.Repeat("?,", len(talkers))
		whereClauses = append(whereClauses, fmt.Sprintf("m.talker IN (%s)", strings.TrimSuffix(placeholders, ",")))
		for _, t := range talkers {
			args = append(args, t)
		}
	}
	if len(senders) > 0 {
		placeholders := strings.Repeat("?,", len(senders))
		whereClauses = append(whereClauses, fmt.Sprintf("m.sender IN (%s)", strings.TrimSuffix(placeholders, ",")))
		for _, s := range senders {
			args = append(args, s)
		}
	}
	if startUnix > 0 {
		whereClauses = append(whereClauses, "m.unix >= ?")
		args = append(args, startUnix)
	}
	if endUnix > 0 {
		whereClauses = append(whereClauses, "m.unix <= ?")
		args = append(args, endUnix)
	}

	baseQuery := strings.Builder{}
	baseQuery.WriteString(`
FROM messages_fts
JOIN messages m ON m.rowid = messages_fts.rowid
WHERE messages_fts MATCH ?
`)
	if len(whereClauses) > 0 {
		baseQuery.WriteString(" AND ")
		baseQuery.WriteString(strings.Join(whereClauses, " AND "))
	}

	countQuery := "SELECT COUNT(*) " + baseQuery.String()

	dataQuery := "SELECT m.message_json, " +
		"COALESCE(snippet(messages_fts, 0, '<mark>', '</mark>', '...', 16), '') AS snippet, " +
		"COALESCE(bm25(messages_fts), 0.0) AS score " +
		baseQuery.String() +
		" ORDER BY score ASC, m.unix DESC, m.seq DESC LIMIT ? OFFSET ?"

	countArgs := append([]interface{}{}, args...)
	dataArgs := append([]interface{}{}, args...)
	dataArgs = append(dataArgs, limit, offset)

	ctx := context.Background()

	var total int
	if err := db.QueryRowContext(ctx, countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search results: %w", err)
	}

	rows, err := db.QueryContext(ctx, dataQuery, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("execute search query: %w", err)
	}
	defer rows.Close()

	hits := make([]*SearchHit, 0)
	for rows.Next() {
		var messageJSON string
		var snippet sql.NullString
		var score sql.NullFloat64
		if err := rows.Scan(&messageJSON, &snippet, &score); err != nil {
			return nil, 0, fmt.Errorf("scan search hit: %w", err)
		}

		var msg model.Message
		if err := json.Unmarshal([]byte(messageJSON), &msg); err != nil {
			return nil, 0, fmt.Errorf("decode message: %w", err)
		}

		hits = append(hits, &SearchHit{
			Message: &msg,
			Snippet: snippet.String,
			Score:   score.Float64,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate search hits: %w", err)
	}

	return hits, total, nil
}

// SearchHit represents a single FTS search hit mapped to the domain model.
type SearchHit struct {
	Message *model.Message
	Snippet string
	Score   float64
}

func loadMetadata(path string) (metadata, error) {
	var meta metadata

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	if len(data) == 0 {
		return meta, nil
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func (i *Index) saveMetadataLocked() error {
	if i == nil {
		return nil
	}

	data, err := json.MarshalIndent(i.meta, "", "  ")
	if err != nil {
		return err
	}

	tmp := i.metaPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}

	return os.Rename(tmp, i.metaPath)
}

func buildFTSQuery(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", nil
	}

	upper := strings.ToUpper(s)
	advanced := strings.ContainsAny(s, "\"'*()") ||
		strings.Contains(upper, " AND ") ||
		strings.Contains(upper, " OR ") ||
		strings.HasPrefix(upper, "NOT ")
	if advanced {
		return s, nil
	}

	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return "", nil
	}

	escaped := make([]string, 0, len(tokens))
	for _, token := range tokens {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		t = strings.ReplaceAll(t, "\"", "\"\"")
		escaped = append(escaped, fmt.Sprintf("\"%s\"", t))
	}

	if len(escaped) == 0 {
		return "", nil
	}

	if len(escaped) == 1 {
		return escaped[0], nil
	}

	return strings.Join(escaped, " AND "), nil
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	return result
}

type document struct {
	ID          string
	Talker      string
	Sender      string
	Unix        int64
	Seq         int64
	Content     string
	MessageJSON string
}

func newDocument(msg *model.Message) (*document, error) {
	if msg == nil {
		return nil, errors.New("nil message")
	}

	content := normalizeContent(msg.PlainTextContent())
	messageJSON, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}

	return &document{
		ID:          fmt.Sprintf("%s:%d", msg.Talker, msg.Seq),
		Talker:      msg.Talker,
		Sender:      msg.Sender,
		Unix:        msg.Time.Unix(),
		Seq:         msg.Seq,
		Content:     content,
		MessageJSON: string(messageJSON),
	}, nil
}

type runeClass int

const (
	runeClassNone runeClass = iota
	runeClassSpace
	runeClassASCII
	runeClassNonASCII
	runeClassOther
)

func classifyRune(r rune) runeClass {
	switch {
	case unicode.IsSpace(r):
		return runeClassSpace
	case r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
		return runeClassASCII
	case unicode.IsLetter(r) || unicode.IsDigit(r):
		return runeClassNonASCII
	default:
		return runeClassOther
	}
}

func normalizeContent(input string) string {
	if input == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(input) + 8)

	prevClass := runeClassNone
	lastWasSpace := false

	writeSpace := func() {
		if !lastWasSpace && b.Len() > 0 {
			b.WriteByte(' ')
			lastWasSpace = true
		}
	}

	for _, r := range input {
		class := classifyRune(r)
		switch class {
		case runeClassSpace:
			writeSpace()
			prevClass = runeClassSpace
			continue
		case runeClassASCII:
			if prevClass == runeClassNonASCII {
				writeSpace()
			}
			b.WriteRune(unicode.ToLower(r))
			lastWasSpace = false
		case runeClassNonASCII:
			if prevClass == runeClassASCII {
				writeSpace()
			}
			b.WriteRune(r)
			lastWasSpace = false
		default:
			switch r {
			case '\n', '\r', '\t':
				writeSpace()
				prevClass = runeClassSpace
				continue
			default:
				b.WriteRune(r)
				lastWasSpace = false
			}
		}
		prevClass = class
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return input
	}
	return result
}
