package repository

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/model"
	"github.com/sjzar/chatlog/internal/wechatdb/indexer"
	"github.com/sjzar/chatlog/internal/wechatdb/msgstore"
	"github.com/sjzar/chatlog/pkg/util"
)

type ftsIndexable interface {
	ListTalkers(ctx context.Context) ([]string, error)
	IterateMessages(ctx context.Context, talkers []string, fn func(*model.Message) error) error
}

func (r *Repository) initIndex() error {
	if r.indexPath == "" {
		return nil
	}

	idx, err := indexer.Open(r.indexPath)
	if err != nil {
		return err
	}

	r.index = idx
	r.indexCtx, r.indexCancel = context.WithCancel(context.Background())

	go func() {
		ready, err := r.ensureIndex(r.indexCtx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Warn().Err(err).Msg("ensure fts index failed")
			return
		}
		if ready {
			log.Info().Msg("fts index ready")
		}
	}()

	return nil
}

func (r *Repository) ensureIndex(ctx context.Context) (bool, error) {
	if r.index == nil {
		return false, nil
	}

	if ctx == nil {
		ctx = context.Background()
	}

	versionMatched, err := r.index.EnsureVersion()
	if err != nil {
		return false, err
	}

	if !versionMatched {
		r.indexMu.Lock()
		r.indexStatus.Ready = false
		r.indexStatus.Progress = 0
		r.indexFingerprint = ""
		r.indexMu.Unlock()
	}

	fp, err := r.ds.GetDatasetFingerprint(ctx)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(fp) == "" {
		return false, fmt.Errorf("dataset fingerprint is empty")
	}

	r.indexMu.Lock()
	if r.indexFingerprint == fp && r.indexStatus.Ready && !r.indexStatus.InProgress {
		status := r.indexStatus
		r.indexMu.Unlock()
		if status.Progress < 1 {
			r.updateIndexProgress(1)
		}
		return true, nil
	}
	if r.indexStatus.InProgress {
		r.indexMu.Unlock()
		return false, nil
	}
	r.indexStatus.InProgress = true
	r.indexStatus.Ready = false
	r.indexStatus.Progress = 0
	r.indexStatus.LastStartedAt = time.Now()
	r.indexStatus.LastError = ""
	r.indexMu.Unlock()

	err = r.rebuildIndex(ctx, fp)
	if err != nil {
		if err == context.Canceled || errors.Is(err, context.Canceled) {
			r.indexMu.Lock()
			r.indexStatus.InProgress = false
			r.indexMu.Unlock()
			return false, err
		}

		r.indexMu.Lock()
		r.indexStatus.InProgress = false
		r.indexStatus.LastError = err.Error()
		r.indexMu.Unlock()
		return false, err
	}

	r.indexMu.Lock()
	r.indexFingerprint = fp
	r.indexStatus.InProgress = false
	r.indexStatus.Ready = true
	r.indexStatus.Progress = 1
	r.indexStatus.LastCompletedAt = time.Now()
	r.indexMu.Unlock()

	return true, nil
}

func (r *Repository) rebuildIndex(ctx context.Context, fp string) error {
	indexable, ok := r.ds.(ftsIndexable)
	if !ok {
		return fmt.Errorf("datasource does not support fts indexing")
	}

	stores, err := r.ds.ListMessageStores(ctx)
	if err != nil {
		return err
	}

	if err := r.index.Reset(); err != nil {
		return err
	}
	if _, err := r.index.EnsureVersion(); err != nil {
		return err
	}
	if err := r.index.SyncStores(stores); err != nil {
		return err
	}

	if len(stores) == 0 {
		if err := r.index.UpdateFingerprint(fp); err != nil {
			return err
		}
		return r.index.UpdateLastBuilt(time.Now())
	}

	storeByID := make(map[string]*msgstore.Store, len(stores))
	storeByPath := make(map[string]*msgstore.Store, len(stores))
	talkerHashStore := make(map[string]*msgstore.Store)
	for _, store := range stores {
		if store == nil {
			continue
		}
		storeByID[store.ID] = store
		if store.FilePath != "" {
			storeByPath[filepath.Clean(store.FilePath)] = store
		}
		for hash := range store.Talkers {
			talkerHashStore[hash] = store
		}
	}

	const perStoreBatchSize = 512
	storeBuffers := make(map[string][]*model.Message, len(stores))
	dirtyStores := make(map[string]struct{})

	locateStore := func(msg *model.Message) (*msgstore.Store, error) {
		if msg == nil {
			return nil, errors.New("message is nil")
		}
		talker := strings.TrimSpace(msg.Talker)
		if talker != "" {
			hashBytes := md5.Sum([]byte(talker))
			hash := hex.EncodeToString(hashBytes[:])
			if store := talkerHashStore[hash]; store != nil {
				return store, nil
			}
		}

		located, err := r.ds.LocateMessageStore(msg)
		if err != nil {
			return nil, err
		}
		if located == nil {
			return nil, fmt.Errorf("message store not found for talker %s", talker)
		}
		if store := storeByPath[filepath.Clean(located.FilePath)]; store != nil {
			return store, nil
		}
		if store := storeByID[located.ID]; store != nil {
			return store, nil
		}
		return nil, fmt.Errorf("message store %s not registered", located.FilePath)
	}

	flushStore := func(store *msgstore.Store) error {
		if store == nil {
			return nil
		}
		buf := storeBuffers[store.ID]
		if len(buf) == 0 {
			return nil
		}
		if err := r.index.IndexStoreMessages(store, buf); err != nil {
			return err
		}
		storeBuffers[store.ID] = buf[:0]
		return nil
	}

	flushDirty := func() error {
		for id := range dirtyStores {
			store := storeByID[id]
			if err := flushStore(store); err != nil {
				return err
			}
			delete(dirtyStores, id)
		}
		return nil
	}

	talkers, err := indexable.ListTalkers(ctx)
	if err != nil {
		return err
	}

	if len(talkers) == 0 {
		if err := flushDirty(); err != nil {
			return err
		}
		if err := r.index.UpdateFingerprint(fp); err != nil {
			return err
		}
		return r.index.UpdateLastBuilt(time.Now())
	}

	sort.Strings(talkers)

	total := float64(len(talkers))
	for i, talker := range talkers {
		if err := ctx.Err(); err != nil {
			return err
		}

		handler := func(msg *model.Message) error {
			if msg == nil {
				return nil
			}
			store, err := locateStore(msg)
			if err != nil {
				log.Warn().Err(err).Str("talker", msg.Talker).Msg("skip message without store")
				return nil
			}
			batch := storeBuffers[store.ID]
			batch = append(batch, msg)
			if len(batch) >= perStoreBatchSize {
				if err := r.index.IndexStoreMessages(store, batch); err != nil {
					return err
				}
				batch = batch[:0]
			}
			storeBuffers[store.ID] = batch
			dirtyStores[store.ID] = struct{}{}
			return nil
		}

		if err := indexable.IterateMessages(ctx, []string{talker}, handler); err != nil {
			return err
		}

		if err := flushDirty(); err != nil {
			return err
		}

		r.updateIndexProgress(float64(i+1) / total)
	}

	if err := flushDirty(); err != nil {
		return err
	}

	if err := r.index.UpdateFingerprint(fp); err != nil {
		return err
	}
	if err := r.index.UpdateLastBuilt(time.Now()); err != nil {
		return err
	}

	return nil
}

func (r *Repository) updateIndexProgress(progress float64) {
	if r.index == nil {
		return
	}

	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}

	r.indexMu.Lock()
	r.indexStatus.Progress = progress
	r.indexMu.Unlock()
}

func (r *Repository) indexStatusSnapshot() *model.SearchIndexStatus {
	if r.index == nil {
		return nil
	}

	r.indexMu.Lock()
	status := r.indexStatus
	r.indexMu.Unlock()

	copied := status
	return &copied
}

func (r *Repository) searchMessagesWithIndex(ctx context.Context, req *model.SearchRequest) (*model.SearchResponse, error) {
	makeEmpty := func() *model.SearchResponse {
		return &model.SearchResponse{
			Total:      0,
			Hits:       []*model.SearchHit{},
			DurationMs: 0,
			Limit:      req.Limit,
			Offset:     req.Offset,
			Query:      req.Query,
			Talker:     req.Talker,
			Sender:     req.Sender,
			Start:      req.Start,
			End:        req.End,
			Index:      r.indexStatusSnapshot(),
		}
	}

	if strings.TrimSpace(req.Query) == "" {
		return makeEmpty(), nil
	}

	if r.index == nil {
		return makeEmpty(), nil
	}

	ready, err := r.ensureIndex(ctx)
	if err != nil {
		return nil, err
	}
	if !ready {
		return makeEmpty(), nil
	}

	talkers := util.Str2List(req.Talker, ",")
	senders := util.Str2List(req.Sender, ",")

	startUnix := int64(0)
	if !req.Start.IsZero() {
		startUnix = req.Start.Unix()
	}
	endUnix := int64(0)
	if !req.End.IsZero() {
		endUnix = req.End.Unix()
	}
	if startUnix > 0 && endUnix > 0 && endUnix < startUnix {
		startUnix, endUnix = endUnix, startUnix
	}

	begin := time.Now()
	hits, total, err := r.index.Search(req, talkers, senders, startUnix, endUnix, req.Offset, req.Limit)
	if err != nil {
		return nil, err
	}

	mapped := make([]*model.SearchHit, 0, len(hits))
	for _, hit := range hits {
		if hit == nil || hit.Message == nil {
			continue
		}
		mapped = append(mapped, &model.SearchHit{
			Message: hit.Message,
			Snippet: hit.Snippet,
			Score:   hit.Score,
		})
	}

	resp := &model.SearchResponse{
		Total:      total,
		Hits:       mapped,
		DurationMs: time.Since(begin).Milliseconds(),
		Limit:      req.Limit,
		Offset:     req.Offset,
		Query:      req.Query,
		Talker:     req.Talker,
		Sender:     req.Sender,
		Start:      req.Start,
		End:        req.End,
		Index:      r.indexStatusSnapshot(),
	}

	return resp, nil
}
