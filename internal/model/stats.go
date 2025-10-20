package model

// GlobalMessageStats 汇总消息统计
type GlobalMessageStats struct {
	Total        int64            `json:"total"`
	Sent         int64            `json:"sent"`
	Received     int64            `json:"received"`
	EarliestUnix int64            `json:"earliest_unix"`
	LatestUnix   int64            `json:"latest_unix"`
	ByType       map[string]int64 `json:"by_type"` // 例如：{"文本":123, "图片":456}
}

// MonthlyTrend 月度趋势
type MonthlyTrend struct {
	Date     string `json:"date"` // YYYY-MM
	Sent     int64  `json:"sent"`
	Received int64  `json:"received"`
}
