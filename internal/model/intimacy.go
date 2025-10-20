package model

// IntimacyBase 聚合每个会话/联系人基础统计，用于计算亲密度
type IntimacyBase struct {
	UserName         string
	MsgCount         int64
	SentCount        int64
	ReceivedCount    int64
	MinCreateUnix    int64
	MaxCreateUnix    int64
	MessagingDays    int64
	Last90DaysMsg    int64
	Past7DaysSentMsg int64
}
