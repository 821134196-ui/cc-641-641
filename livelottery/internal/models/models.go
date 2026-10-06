package models

import (
	"encoding/json"
	"time"
)

// 活动状态机：draft -> locked -> ended -> drawn（单向，不可逆）
const (
	StatusDraft  = "draft"
	StatusLocked = "locked" // 规则/库存冻结，开放报名
	StatusEnded  = "ended"  // 直播结束，等待核验资格与开奖
	StatusDrawn  = "drawn"  // 已公布唯一正式名单
)

// 报名资格状态
const (
	RegRegistered = "registered" // 已报名占名额
	RegInReview   = "in_review"  // 退货事件触发复核
	RegEligible   = "eligible"   // 复核通过 / 核验合格
	RegIneligible = "ineligible" // 复核判定失效
)

// 中奖记录状态
const (
	WinPending      = "pending"
	WinConfirmed    = "confirmed" // 已确认并原子扣减库存、占用具体奖品编号
	WinForfeited    = "forfeited" // 超期未填地址，按锁定规则判弃奖
	WinReviewing    = "reviewing" // 开奖后发生退货，进入复核
	WinDisqualified = "disqualified"
	WinShipped      = "shipped"
	WinDelivered    = "delivered"
)

// 奖品编号（单件库存）状态
const (
	ItemAvailable = "available"
	ItemClaimed   = "claimed" // 已被中奖确认原子占用
	ItemShipped   = "shipped"
	ItemReleased  = "released" // 弃奖/取消后释放（不补抽）
)

// 发货单状态
const (
	ShipPending   = "pending"
	ShipShipped   = "shipped"
	ShipDelivered = "delivered"
	ShipCancelled = "cancelled"
)

// RuleSnapshot 是活动锁定时固化的规则，锁定后任何接口不得修改。
type RuleSnapshot struct {
	MinAccountAgeDays int  `json:"min_account_age_days"`
	NeedBoundPhone    bool `json:"need_bound_phone"`

	WinnerCount int `json:"winner_count"`

	AddressDeadlineHours float64 `json:"address_deadline_hours"`
	OnAddressTimeout     string  `json:"on_address_timeout"` // forfeit：弃奖、释放库存、不补抽

	ReturnInvalidatesEligibility bool `json:"return_invalidates_eligibility"`

	Algorithm string `json:"algorithm"`
}

func (r RuleSnapshot) Encode() string {
	b, _ := json.Marshal(r)
	return string(b)
}

type Activity struct {
	ID       int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	Name     string     `gorm:"not null" json:"name"`
	Status   string     `gorm:"not null;index;default:draft" json:"status"`
	DrawAt   time.Time  `json:"draw_at"`
	LockedAt *time.Time `json:"locked_at,omitempty"`
	EndedAt  *time.Time `json:"ended_at,omitempty"`
	DrawnAt  *time.Time `json:"drawn_at,omitempty"`

	// 锁定的规则快照（冻结文本，仅供审计查阅）
	RuleJSON string `gorm:"not null;default:''" json:"rule_json"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (a Activity) Rule() (RuleSnapshot, error) {
	var r RuleSnapshot
	if a.RuleJSON == "" {
		return r, nil
	}
	err := json.Unmarshal([]byte(a.RuleJSON), &r)
	return r, err
}

// Prize 奖品（一种奖品对应一批编号库存）
type Prize struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	ActivityID int64     `gorm:"not null;index" json:"activity_id"`
	Name       string    `gorm:"not null" json:"name"`
	TotalStock int       `gorm:"not null;default:0" json:"total_stock"` // 锁定时固化
	CreatedAt  time.Time `json:"created_at"`
}

// PrizeItem 奖品编号：每件真实可发库存一行，状态机保证不超扣
type PrizeItem struct {
	ID       int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	PrizeID  int64  `gorm:"not null;uniqueIndex:idx_prize_serial" json:"prize_id"`
	SerialNo string `gorm:"not null;uniqueIndex:idx_prize_serial" json:"serial_no"`
	Status   string `gorm:"not null;index;default:available" json:"status"`

	// 一件奖品最多归属一个中奖者
	WinnerRecordID *int64 `gorm:"uniqueIndex" json:"winner_record_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Registration 报名记录：同一活动同一账号只能占一个名额
type Registration struct {
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	ActivityID int64  `gorm:"not null;uniqueIndex:idx_reg_account" json:"activity_id"`
	AccountID  string `gorm:"not null;uniqueIndex:idx_reg_account" json:"account_id"`
	OrderNo    string `gorm:"not null;default:''" json:"order_no"`
	Status     string `gorm:"not null;index;default:registered" json:"status"`
	ReviewNote string `gorm:"not null;default:''" json:"review_note"`

	// 报名当时用于核验参加条件的证据快照（模拟直播账号资料）
	AccountAgeDays int  `gorm:"not null;default:0" json:"account_age_days"`
	PhoneBound     bool `gorm:"not null;default:false" json:"phone_bound"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DrawBatch 开奖批次：activity_id 唯一索引从数据库层保证“唯一正式名单”。
type DrawBatch struct {
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	ActivityID int64  `gorm:"not null;uniqueIndex" json:"activity_id"`
	BatchCode  string `gorm:"not null;uniqueIndex" json:"batch_code"`
	Seed       int64  `gorm:"not null" json:"seed"`
	Algorithm  string `gorm:"not null" json:"algorithm"`

	// 开奖时合格报名 ID 的冻结快照，用于离线复核原结果
	EligibleIDsJSON string `gorm:"not null;default:'[]'" json:"-"`
	// 正式名单结果哈希：有序账号列表 sha256，重复开奖/篡改可验
	ResultHash string `gorm:"not null" json:"result_hash"`

	CreatedAt time.Time `json:"created_at"`
}

// WinnerRecord 正式名单行
type WinnerRecord struct {
	ID          int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	DrawBatchID int64  `gorm:"not null;uniqueIndex:idx_winner_account" json:"draw_batch_id"`
	ActivityID  int64  `gorm:"not null;index" json:"activity_id"`
	AccountID   string `gorm:"not null;uniqueIndex:idx_winner_account" json:"account_id"`
	Rank        int    `gorm:"not null" json:"rank"`
	PrizeID     int64  `gorm:"not null" json:"prize_id"`
	Status      string `gorm:"not null;index" json:"status"`

	Receiver    string     `gorm:"not null;default:''" json:"receiver"`
	Phone       string     `gorm:"not null;default:''" json:"phone"`
	Address     string     `gorm:"not null;default:''" json:"address"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	ForfeitedAt *time.Time `json:"forfeited_at,omitempty"`

	// 原子扣减时占用的具体奖品编号
	PrizeItemID *int64 `gorm:"uniqueIndex" json:"prize_item_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Shipment 发货单：一个中奖记录至多一张，一个奖品编号至多出库一次
type Shipment struct {
	ID             int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	WinnerRecordID int64      `gorm:"not null;uniqueIndex" json:"winner_record_id"`
	PrizeItemID    int64      `gorm:"not null;uniqueIndex" json:"prize_item"`
	TrackingNo     string     `gorm:"not null;uniqueIndex" json:"tracking_no"`
	Status         string     `gorm:"not null;default:pending" json:"status"`
	ShippedAt      *time.Time `json:"shipped_at,omitempty"`
	DeliveredAt    *time.Time `json:"delivered_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// LogisticsReceipt 物流回执：receipt_no 唯一，重复回执幂等忽略
type LogisticsReceipt struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	ReceiptNo  string    `gorm:"not null;uniqueIndex" json:"receipt_no"`
	ShipmentID int64     `gorm:"not null;index" json:"shipment_id"`
	EventType  string    `gorm:"not null" json:"event_type"` // shipped / delivered
	CreatedAt  time.Time `json:"created_at"`
}

// StockLedger 奖品流水：每次库存变动一行，含变动后余额
type StockLedger struct {
	ID           int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	PrizeID      int64     `gorm:"not null;index" json:"prize_id"`
	Change       int       `gorm:"not null" json:"change"`
	BalanceAfter int       `gorm:"not null" json:"balance_after"`
	Reason       string    `gorm:"not null" json:"reason"`
	RefType      string    `gorm:"not null;default:''" json:"ref_type"`
	RefID        int64     `gorm:"not null;default:0" json:"ref_id"`
	CreatedAt    time.Time `json:"created_at"`
}

// AuditLog 操作审计
type AuditLog struct {
	ID         int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	ActivityID int64     `gorm:"not null;index" json:"activity_id"`
	Actor      string    `gorm:"not null;default:''" json:"actor"`
	Action     string    `gorm:"not null" json:"action"`
	Detail     string    `gorm:"not null;default:''" json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

func AllModels() []any {
	return []any{
		&Activity{}, &Prize{}, &PrizeItem{}, &Registration{},
		&DrawBatch{}, &WinnerRecord{}, &Shipment{},
		&LogisticsReceipt{}, &StockLedger{}, &AuditLog{},
	}
}
