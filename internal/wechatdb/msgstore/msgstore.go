package msgstore

import (
	"context"
	"time"

	"github.com/sjzar/chatlog/internal/model"
)

// Store describes a physical message database and its derived FTS index metadata.
type Store struct {
	ID        string
	FilePath  string
	FileName  string
	IndexPath string
	StartTime time.Time
	EndTime   time.Time
	Talkers   map[string]struct{}
}

// Clone creates a shallow copy of the store with a deep copy of the talker set.
func (s *Store) Clone() *Store {
	if s == nil {
		return nil
	}
	clone := *s
	if s.Talkers != nil {
		talkers := make(map[string]struct{}, len(s.Talkers))
		for talker := range s.Talkers {
			talkers[talker] = struct{}{}
		}
		clone.Talkers = talkers
	}
	return &clone
}

// Provider exposes message store metadata for building per-database indexes.
type Provider interface {
	ListMessageStores(ctx context.Context) ([]*Store, error)
	LocateMessageStore(msg *model.Message) (*Store, error)
}
