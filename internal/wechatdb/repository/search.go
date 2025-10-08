package repository

import (
	"context"

	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/model"
)

// SearchMessages 执行全文检索，并在返回前补充联系人/群聊信息。
func (r *Repository) SearchMessages(ctx context.Context, req *model.SearchRequest) (*model.SearchResponse, error) {
	if req == nil {
		return nil, errors.InvalidArg("request")
	}

	nReq := req.Clone()
	if nReq == nil {
		nReq = &model.SearchRequest{}
	}

	// 兼容现有的联系人/群聊别名：在进入数据源前将 talker/sender 解析成真实 userName
	normalizedTalker, normalizedSender := r.parseTalkerAndSender(ctx, nReq.Talker, nReq.Sender)
	nReq.Talker = normalizedTalker
	nReq.Sender = normalizedSender

	resp, err := r.ds.SearchMessages(ctx, nReq)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &model.SearchResponse{Hits: []*model.SearchHit{}, Limit: nReq.Limit, Offset: nReq.Offset}, nil
	}

	// Enrich message metadata（头像、群昵称、显示名等）
	messages := make([]*model.Message, 0, len(resp.Hits))
	for _, hit := range resp.Hits {
		if hit == nil || hit.Message == nil {
			continue
		}
		messages = append(messages, hit.Message)
	}

	if len(messages) > 0 {
		if err := r.EnrichMessages(ctx, messages); err != nil {
			log.Debug().Msgf("EnrichMessages in search failed: %v", err)
		}
	}

	return resp, nil
}
