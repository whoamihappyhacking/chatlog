package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/sjzar/chatlog/internal/model"
)

const (
	runtimeIndexVersionKey = "chatlog_index_version"
	runtimeIndexVersion    = "1"
	fingerprintKey         = "chatlog_index_fingerprint"
	lastBuiltKey           = "chatlog_index_last_built"
)

// SearchHit represents a single Bleve search hit mapped back to chatlog's domain model.
type SearchHit struct {
	Message *model.Message
	Snippet string
	Score   float64
}

// Index wraps a Bleve index with concurrency control and helper utilities.
type Index struct {
	mu   sync.RWMutex
	idx  bleve.Index
	path string
}

// Open opens an existing index at the given path or creates a new one with the default mapping.
func Open(path string) (*Index, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create index parent dir: %w", err)
	}

	var (
		idx bleve.Index
		err error
	)
	if _, statErr := os.Stat(path); statErr == nil {
		idx, err = bleve.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open bleve index: %w", err)
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		mapping, mapErr := buildMapping()
		if mapErr != nil {
			return nil, mapErr
		}
		idx, err = bleve.New(path, mapping)
		if err != nil {
			return nil, fmt.Errorf("create bleve index: %w", err)
		}
	} else {
		return nil, fmt.Errorf("stat index: %w", statErr)
	}

	return &Index{idx: idx, path: path}, nil
}

// Close closes the underlying Bleve index.
func (i *Index) Close() error {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.idx == nil {
		return nil
	}
	return i.idx.Close()
}

// Reset drops all existing data and recreates the index with the default mapping.
func (i *Index) Reset() error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.idx != nil {
		_ = i.idx.Close()
	}
	if err := os.RemoveAll(i.path); err != nil {
		return fmt.Errorf("remove index dir: %w", err)
	}
	mapping, err := buildMapping()
	if err != nil {
		return err
	}
	idx, err := bleve.New(i.path, mapping)
	if err != nil {
		return fmt.Errorf("recreate bleve index: %w", err)
	}
	i.idx = idx
	return nil
}

// SetMetadata stores an internal key/value pair within the index (not searchable).
func (i *Index) SetMetadata(key string, value []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.idx == nil {
		return errors.New("index not initialized")
	}
	return i.idx.SetInternal([]byte(key), value)
}

// GetMetadata retrieves an internal key/value pair.
func (i *Index) GetMetadata(key string) ([]byte, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.idx == nil {
		return nil, errors.New("index not initialized")
	}
	return i.idx.GetInternal([]byte(key))
}

// IndexMessages indexes a batch of messages using Bleve batches for efficiency.
func (i *Index) IndexMessages(messages []*model.Message) error {
	if len(messages) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.idx == nil {
		return errors.New("index not initialized")
	}

	batch := i.idx.NewBatch()
	const batchSize = 250
	for idx, msg := range messages {
		doc, err := newDocument(msg)
		if err != nil {
			return err
		}
		if err := batch.Index(doc.ID, doc); err != nil {
			return fmt.Errorf("batch index: %w", err)
		}
		if (idx+1)%batchSize == 0 {
			if err := i.idx.Batch(batch); err != nil {
				return fmt.Errorf("flush batch: %w", err)
			}
			batch = i.idx.NewBatch()
		}
	}

	if batch.Size() > 0 {
		if err := i.idx.Batch(batch); err != nil {
			return fmt.Errorf("flush final batch: %w", err)
		}
	}

	return nil
}

// Search executes a Bleve search over the indexed content applying optional filters.
func (i *Index) Search(req *model.SearchRequest, talkers []string, senders []string, startUnix, endUnix int64, offset, limit int) ([]*SearchHit, int, error) {
	if req == nil {
		return nil, 0, errors.New("search request is nil")
	}

	queryObj := buildQuery(req.Query, talkers, senders, startUnix, endUnix)
	if queryObj == nil {
		return []*SearchHit{}, 0, nil
	}

	i.mu.RLock()
	idx := i.idx
	i.mu.RUnlock()
	if idx == nil {
		return nil, 0, errors.New("index not initialized")
	}

	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	searchRequest := bleve.NewSearchRequestOptions(queryObj, limit, offset, false)
	searchRequest.Highlight = bleve.NewHighlightWithStyle("html")
	searchRequest.Fields = []string{"message_json"}
	searchRequest.IncludeLocations = false

	result, err := idx.Search(searchRequest)
	if err != nil {
		return nil, 0, fmt.Errorf("bleve search: %w", err)
	}

	hits := make([]*SearchHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		messageJSON, ok := hit.Fields["message_json"].(string)
		if !ok || messageJSON == "" {
			continue
		}
		var msg model.Message
		if err := json.Unmarshal([]byte(messageJSON), &msg); err != nil {
			return nil, 0, fmt.Errorf("decode message: %w", err)
		}

		snippet := ""
		if frags, ok := hit.Fragments["content"]; ok && len(frags) > 0 {
			snippet = strings.Join(frags, " … ")
		}

		hits = append(hits, &SearchHit{
			Message: &msg,
			Snippet: snippet,
			Score:   hit.Score,
		})
	}

	return hits, int(result.Total), nil
}

// Document representation stored inside Bleve.
type document struct {
	ID          string `json:"id"`
	Talker      string `json:"talker"`
	Sender      string `json:"sender"`
	Unix        int64  `json:"unix"`
	Seq         int64  `json:"seq"`
	Content     string `json:"content"`
	MessageJSON string `json:"message_json"`
}

func newDocument(msg *model.Message) (*document, error) {
	if msg == nil {
		return nil, errors.New("nil message")
	}
	content := msg.PlainTextContent()
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

func buildMapping() (*mapping.IndexMappingImpl, error) {
	indexMapping := bleve.NewIndexMapping()
	indexMapping.TypeField = "type"
	indexMapping.DefaultAnalyzer = "standard"

	docMapping := mapping.NewDocumentMapping()

	contentField := mapping.NewTextFieldMapping()
	contentField.Analyzer = "standard"
	contentField.Store = false
	docMapping.AddFieldMappingsAt("content", contentField)

	talkerField := mapping.NewTextFieldMapping()
	talkerField.Analyzer = "keyword"
	talkerField.Store = true
	talkerField.IncludeInAll = false
	docMapping.AddFieldMappingsAt("talker", talkerField)

	senderField := mapping.NewTextFieldMapping()
	senderField.Analyzer = "keyword"
	senderField.Store = true
	senderField.IncludeInAll = false
	docMapping.AddFieldMappingsAt("sender", senderField)

	unixField := mapping.NewNumericFieldMapping()
	unixField.Store = true
	docMapping.AddFieldMappingsAt("unix", unixField)

	seqField := mapping.NewNumericFieldMapping()
	seqField.Store = true
	docMapping.AddFieldMappingsAt("seq", seqField)

	messageField := mapping.NewTextFieldMapping()
	messageField.Analyzer = "keyword"
	messageField.Store = true
	messageField.Index = false
	docMapping.AddFieldMappingsAt("message_json", messageField)

	indexMapping.DefaultMapping = docMapping

	return indexMapping, nil
}

func buildQuery(input string, talkers []string, senders []string, startUnix, endUnix int64) query.Query {
	contentQuery := buildContentQuery(input)

	var must []query.Query
	if contentQuery != nil {
		must = append(must, contentQuery)
	}

	if len(talkers) > 0 {
		must = append(must, buildTermsFilter("talker", talkers))
	}
	if len(senders) > 0 {
		must = append(must, buildTermsFilter("sender", senders))
	}
	if startUnix > 0 || endUnix > 0 {
		var minPtr, maxPtr *float64
		if startUnix > 0 {
			min := float64(startUnix)
			minPtr = &min
		}
		if endUnix > 0 {
			max := float64(endUnix)
			maxPtr = &max
		}
		rangeQuery := query.NewNumericRangeQuery(minPtr, maxPtr)
		rangeQuery.SetField("unix")
		must = append(must, rangeQuery)
	}

	if len(must) == 0 {
		return nil
	}

	if len(must) == 1 {
		return must[0]
	}

	return query.NewConjunctionQuery(must)
}

func buildContentQuery(input string) query.Query {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil
	}

	upper := strings.ToUpper(s)
	advanced := strings.ContainsAny(s, "\"'*()") ||
		strings.Contains(upper, " AND ") ||
		strings.Contains(upper, " OR ") ||
		strings.Contains(upper, " NEAR ") ||
		strings.HasPrefix(upper, "NOT ")

	if advanced {
		return query.NewQueryStringQuery(s)
	}

	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return nil
	}

	if len(tokens) == 1 {
		mq := query.NewMatchQuery(tokens[0])
		mq.SetField("content")
		return mq
	}

	conj := make([]query.Query, 0, len(tokens))
	for _, token := range tokens {
		t := strings.TrimSpace(token)
		if t == "" {
			continue
		}
		mq := query.NewMatchQuery(t)
		mq.SetField("content")
		conj = append(conj, mq)
	}

	if len(conj) == 0 {
		return nil
	}
	if len(conj) == 1 {
		return conj[0]
	}
	return query.NewConjunctionQuery(conj)
}

func buildTermsFilter(field string, values []string) query.Query {
	sanitized := make([]query.Query, 0, len(values))
	for _, val := range values {
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			continue
		}
		tq := query.NewTermQuery(trimmed)
		tq.SetField(field)
		sanitized = append(sanitized, tq)
	}
	if len(sanitized) == 0 {
		return nil
	}
	if len(sanitized) == 1 {
		return sanitized[0]
	}
	return query.NewDisjunctionQuery(sanitized)
}

// EnsureVersion ensures the stored index version matches current runtime version.
func (i *Index) EnsureVersion() (bool, error) {
	current, _ := i.GetMetadata(runtimeIndexVersionKey)
	if string(current) == runtimeIndexVersion {
		return true, nil
	}
	if err := i.SetMetadata(runtimeIndexVersionKey, []byte(runtimeIndexVersion)); err != nil {
		return false, err
	}
	return false, nil
}

// UpdateFingerprint persists the dataset fingerprint into index metadata.
func (i *Index) UpdateFingerprint(fp string) error {
	return i.SetMetadata(fingerprintKey, []byte(fp))
}

// Fingerprint reads the stored dataset fingerprint from index metadata.
func (i *Index) Fingerprint() string {
	b, err := i.GetMetadata(fingerprintKey)
	if err != nil {
		return ""
	}
	return string(b)
}

// FingerprintMatches reports whether the stored dataset fingerprint equals the provided value.
func (i *Index) FingerprintMatches(fp string) bool {
	if fp == "" {
		return false
	}
	return i.Fingerprint() == fp
}

// EnsureFingerprint compares the stored fingerprint with the provided value.
// It returns true when the fingerprint already matches. When it differs, the
// metadata is updated to the new fingerprint and false is returned to signal
// callers that a rebuild just occurred.
func (i *Index) EnsureFingerprint(fp string) (bool, error) {
	if fp == "" {
		return false, nil
	}
	current := i.Fingerprint()
	if current == fp {
		return true, nil
	}
	if err := i.UpdateFingerprint(fp); err != nil {
		return false, err
	}
	return false, nil
}

// UpdateLastBuilt 记录最近一次索引构建完成时间（Unix 秒）
func (i *Index) UpdateLastBuilt(t time.Time) error {
	return i.SetMetadata(lastBuiltKey, []byte(strconv.FormatInt(t.Unix(), 10)))
}

// LastBuilt 返回最近一次索引构建完成时间
func (i *Index) LastBuilt() time.Time {
	b, err := i.GetMetadata(lastBuiltKey)
	if err != nil || len(b) == 0 {
		return time.Time{}
	}
	sec, parseErr := strconv.ParseInt(string(b), 10, 64)
	if parseErr != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}
