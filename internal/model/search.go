package model

import "time"

// SearchRequest 表示一次搜索查询的参数
// Start 和 End 为闭区间；如未提供则由调用方决定默认范围
// Limit/Offset 由调用链路在进入数据源前进行裁剪
// Sender 使用英文逗号分隔多个筛选条件
// Talker 可选：留空时后端会遍历所有会话；如需限定多个会话，使用英文逗号分隔
type SearchRequest struct {
	Query  string    `json:"query"`
	Talker string    `json:"talker"`
	Sender string    `json:"sender"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// Clone 生成请求的浅拷贝，便于在不同层级添加额外参数
func (r *SearchRequest) Clone() *SearchRequest {
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}

// SearchHit 表示一次搜索命中的消息及其高亮片段
// Score 使用 SQLite FTS5 的 bm25 分值，越小代表相关度越高
type SearchHit struct {
	Message *Message `json:"message"`
	Snippet string   `json:"snippet"`
	Score   float64  `json:"score"`
}

// SearchResponse 汇总搜索结果
// DurationMs 统计搜索耗时（毫秒），仅供参考
// Limit / Offset 为实际生效的分页参数
// Hits 序列按相关度排序，命中数可能小于 limit（例如过滤后不足）
type SearchResponse struct {
	Total      int                `json:"total"`
	Hits       []*SearchHit       `json:"hits"`
	DurationMs int64              `json:"duration_ms"`
	Limit      int                `json:"limit"`
	Offset     int                `json:"offset"`
	Query      string             `json:"query"`
	Talker     string             `json:"talker"`
	Sender     string             `json:"sender"`
	Start      time.Time          `json:"start"`
	End        time.Time          `json:"end"`
	Index      *SearchIndexStatus `json:"index_status,omitempty"`
}

// SearchIndexStatus 表示全文索引的构建状态
type SearchIndexStatus struct {
	Ready           bool      `json:"ready"`
	InProgress      bool      `json:"in_progress"`
	Progress        float64   `json:"progress"`
	LastStartedAt   time.Time `json:"last_started_at"`
	LastCompletedAt time.Time `json:"last_completed_at"`
	LastError       string    `json:"last_error,omitempty"`
}
