package models

import "time"

// 活动状态机：draft -> locked -> open -> closed -> drawn -> settled
// 其中 locked 之后规则与库存不可修改；drawn 之后正式名单不可覆盖。
const (
	StatusDraft   = "draft"
	StatusLocked  = "locked"
	StatusOpen    = "open"
	StatusClosed  = "closed"
	StatusDrawn   = "drawn"
	StatusSettled = "settled"
)

// 报名/中奖条目状态
const (
	EntryPending    = "pending"     // 已中奖，待中奖者确认
	EntryConfirmed  = "confirmed"   // 已确认且库存已原子扣减
	EntryAddressed  = "addressed"   // 已填写收货地址
	EntryShipped    = "shipped"     // 已按奖品编号发货
	EntryDelivered  = "delivered"   // 物流妥投
	EntryForfeited  = "forfeited"   // 超期未确认/未填地址，按锁定规则放弃
	EntryVoidReview = "void_review" // 开奖后资格复核失效（名单保留，标记作废，不补抽）
)

// 奖品单价物状态
const (
	UnitStock     = "stock"
	UnitAllocated = "allocated"
	UnitShipped   = "shipped"
	UnitDelivered = "delivered"
	UnitRecovered = "recovered" // 放弃/复核收回，永不再次发放
)

const (
	ShipmentPending   = "pending"
	ShipmentShipped   = "shipped"
	ShipmentDelivered = "delivered"
)

// Campaign 直播抽奖活动
type Campaign struct {
	ID              uint      `gorm:"primarykey" json:"id"`
	Name            string    `gorm:"not null" json:"name"`
	Status          string    `gorm:"not null;index;default:draft" json:"status"`
	ScheduledDrawAt time.Time `json:"scheduled_draw_at"`
	StartAt         time.Time `json:"start_at"`
	EndAt           time.Time `json:"end_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// RuleSnapshot 锁定时刻的规则与库存快照。锁定后不可变。
type RuleSnapshot struct {
	ID         uint `gorm:"primarykey" json:"id"`
	CampaignID uint `gorm:"uniqueIndex;not null" json:"campaign_id"`
	Version    int  `gorm:"not null;default:1" json:"version"`
	// 参与条件：活动期间有效（未退货）订单数 >= MinActiveOrders
	MinActiveOrders int `json:"min_active_orders"`
	// 开奖规则
	WinCount  int    `json:"win_count"`
	Algorithm string `json:"algorithm"` // seeded_fisher_yates
	// 超期未填地址的处理规则（锁定后不可改）
	AddressLimitMinutes int    `json:"address_limit_minutes"`
	ForfeitPolicy       string `json:"forfeit_policy"` // forfeit_no_redraw
	// 库存与名单的审计哈希
	PrizeInventoryHash string    `json:"prize_inventory_hash"`
	RuleHash           string    `json:"rule_hash"`
	LockedAt           time.Time `json:"locked_at"`
}

// Prize 奖项定义与库存计数。remaining 的扣减必须带条件更新。
type Prize struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	CampaignID uint      `gorm:"not null;index" json:"campaign_id"`
	Code       string    `gorm:"not null" json:"code"`
	Name       string    `gorm:"not null" json:"name"`
	TotalStock int       `gorm:"not null" json:"total_stock"`
	Remaining  int       `gorm:"not null" json:"remaining"`
	CreatedAt  time.Time `json:"created_at"`
}

// PrizeUnit 奖品单件，带全局唯一编号（运营按编号发货）。
type PrizeUnit struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	PrizeID    uint      `gorm:"not null;index" json:"prize_id"`
	CampaignID uint      `gorm:"not null;index" json:"campaign_id"`
	SerialNo   string    `gorm:"uniqueIndex;not null" json:"serial_no"`
	Status     string    `gorm:"not null;index;default:stock" json:"status"`
	WinnerID   *uint     `gorm:"index" json:"winner_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Account 模拟观众账号
type Account struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	Nickname  string    `gorm:"not null" json:"nickname"`
	UniqueNo  string    `gorm:"uniqueIndex;not null" json:"unique_no"`
	CreatedAt time.Time `json:"created_at"`
}

// Order 模拟电商订单，退货后使资格失效进入复核
type Order struct {
	ID         uint       `gorm:"primarykey" json:"id"`
	CampaignID uint       `gorm:"not null;index" json:"campaign_id"`
	AccountID  uint       `gorm:"not null;index" json:"account_id"`
	SKU        string     `gorm:"not null" json:"sku"`
	AmountFen  int        `gorm:"not null" json:"amount_fen"`
	Status     string     `gorm:"not null;index;default:active" json:"status"` // active|returned
	CreatedAt  time.Time  `json:"created_at"`
	ReturnedAt *time.Time `json:"returned_at,omitempty"`
}

// Registration 报名记录：(campaign_id, account_id) 唯一，重复报名被数据库拒绝。
type Registration struct {
	ID            uint      `gorm:"primarykey" json:"id"`
	CampaignID    uint      `gorm:"not null;uniqueIndex:uniq_reg_account" json:"campaign_id"`
	AccountID     uint      `gorm:"not null;uniqueIndex:uniq_reg_account" json:"account_id"`
	Qualified     *bool     `gorm:"index" json:"qualified,omitempty"` // 结束后核验：true/false；未核验为 nil
	DisableReason string    `gorm:"size:200" json:"disable_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	Account       *Account  `gorm:"-" json:"account,omitempty"`
}

// DrawBatch 一场活动唯一的一份正式开奖结果。campaign_id 唯一索引物理保证不可生成第二份。
type DrawBatch struct {
	ID            uint      `gorm:"primarykey" json:"id"`
	CampaignID    uint      `gorm:"uniqueIndex;not null" json:"campaign_id"`
	Seed          string    `gorm:"not null" json:"seed"`
	Algorithm     string    `gorm:"not null" json:"algorithm"`
	RuleHash      string    `gorm:"not null" json:"rule_hash"`
	QualifiedRoot string    `gorm:"not null" json:"qualified_root"` // 合格名单哈希
	QualifiedIDs  string    `gorm:"not null" json:"qualified_ids"`  // 排序后的合格账号ID快照（JSON）
	EntryCount    int       `gorm:"not null" json:"entry_count"`
	DrawnAt       time.Time `gorm:"drawn_at"`
}

// DrawEntry 正式名单条目（即中奖记录）。开奖后不可删除，只可追加状态。
type DrawEntry struct {
	ID            uint       `gorm:"primarykey" json:"id"`
	BatchID       uint       `gorm:"not null;uniqueIndex:uniq_entry_rank" json:"batch_id"`
	Rank          int        `gorm:"not null;uniqueIndex:uniq_entry_rank" json:"rank"`
	CampaignID    uint       `gorm:"not null;index" json:"campaign_id"`
	AccountID     uint       `gorm:"not null;index" json:"account_id"`
	PrizeID       uint       `gorm:"not null;index" json:"prize_id"`
	PrizeUnitID   *uint      `gorm:"uniqueIndex" json:"prize_unit_id,omitempty"`
	Status        string     `gorm:"not null;index;default:pending" json:"status"`
	Receiver      string     `gorm:"size:100" json:"receiver,omitempty"`
	Phone         string     `gorm:"size:50" json:"phone,omitempty"`
	Address       string     `gorm:"size:300" json:"address,omitempty"`
	ForfeitReason string     `gorm:"size:200" json:"forfeit_reason,omitempty"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	AddressedAt   *time.Time `json:"addressed_at,omitempty"`
	ForfeitedAt   *time.Time `json:"forfeited_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Account       *Account   `gorm:"-" json:"account,omitempty"`
	PrizeUnit     *PrizeUnit `gorm:"-" json:"prize_unit,omitempty"`
}

// InventoryTransaction 奖品库存流水，每一次变动均可审计。
type InventoryTransaction struct {
	ID           uint      `gorm:"primarykey" json:"id"`
	CampaignID   uint      `gorm:"not null;index" json:"campaign_id"`
	PrizeID      uint      `gorm:"not null;index" json:"prize_id"`
	Type         string    `gorm:"not null" json:"type"` // lock|allocate|recover
	Delta        int       `gorm:"not null" json:"delta"`
	BalanceAfter int       `gorm:"not null" json:"balance_after"`
	RefType      string    `gorm:"size:40" json:"ref_type,omitempty"`
	RefID        uint      `json:"ref_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Shipment 发货单：一个中奖条目最多一张发货单。
type Shipment struct {
	ID          uint       `gorm:"primarykey" json:"id"`
	CampaignID  uint       `gorm:"not null;index" json:"campaign_id"`
	EntryID     uint       `gorm:"uniqueIndex;not null" json:"entry_id"`
	PrizeUnitID uint       `gorm:"uniqueIndex;not null" json:"prize_unit_id"`
	SerialNo    string     `gorm:"not null" json:"serial_no"`
	ShipmentNo  string     `gorm:"uniqueIndex;not null" json:"shipment_no"`
	Status      string     `gorm:"not null;index;default:shipped" json:"status"`
	ShippedAt   time.Time  `json:"shipped_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// LogisticsReceipt 物流回执：回执号唯一，重复回执天然幂等。
type LogisticsReceipt struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	ReceiptNo  string    `gorm:"uniqueIndex;not null" json:"receipt_no"`
	ShipmentID uint      `gorm:"not null;index" json:"shipment_id"`
	Event      string    `gorm:"not null" json:"event"` // accepted|delivered
	Payload    string    `gorm:"size:500" json:"payload,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ReviewCase 资格复核单（如开奖后订单退货）。只记录与处置，绝不触发重新开奖。
type ReviewCase struct {
	ID         uint   `gorm:"primarykey" json:"id"`
	CampaignID uint   `gorm:"not null;index" json:"campaign_id"`
	AccountID  uint   `gorm:"not null;index" json:"account_id"`
	OrderID    uint   `gorm:"not null" json:"order_id"`
	EntryID    *uint  `gorm:"index" json:"entry_id,omitempty"`
	Reason     string `gorm:"not null" json:"reason"`
	Status     string `gorm:"not null;index;default:open" json:"status"` // open|resolved
	Resolution string `gorm:"size:40" json:"resolution,omitempty"`
	// disqualified_before_draw | void_pending | forfeit_recovered | recorded_after_delivery
	Note       string     `gorm:"size:300" json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// AuditLog 审计日志
type AuditLog struct {
	ID         uint      `gorm:"primarykey" json:"id"`
	CampaignID uint      `gorm:"not null;index" json:"campaign_id"`
	Actor      string    `gorm:"size:60;not null;default:system" json:"actor"`
	Action     string    `gorm:"size:60;not null" json:"action"`
	Detail     string    `gorm:"size:1000" json:"detail"`
	CreatedAt  time.Time `json:"created_at"`
}

func AllModels() []any {
	return []any{
		&Campaign{}, &RuleSnapshot{}, &Prize{}, &PrizeUnit{}, &Account{}, &Order{},
		&Registration{}, &DrawBatch{}, &DrawEntry{}, &InventoryTransaction{},
		&Shipment{}, &LogisticsReceipt{}, &ReviewCase{}, &AuditLog{},
	}
}
