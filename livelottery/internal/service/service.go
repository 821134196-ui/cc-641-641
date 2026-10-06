package service

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// 业务错误：API 层据此返回 4xx，而不是 500。
var (
	ErrNotFound           = errors.New("资源不存在")
	ErrInvalidState       = errors.New("当前活动状态不允许该操作")
	ErrRuleLocked         = errors.New("规则已锁定，不能修改")
	ErrAlreadyRegistered  = errors.New("该账号已报名，不能重复占用名额")
	ErrNotEligible        = errors.New("不满足锁定的参加条件")
	ErrRegistrationClosed = errors.New("当前不在报名时段")
	ErrAlreadyDrawn       = errors.New("正式名单已存在，不能重复开奖")
	ErrNotDrawn           = errors.New("尚未开奖")
	ErrWinnerNotPending   = errors.New("该中奖记录不是待确认状态")
	ErrAddressTimeout     = errors.New("已超过锁定规则规定的填写地址时限，按弃奖处理")
	ErrInReview           = errors.New("资格复核中，操作暂缓")
	ErrNoStock            = errors.New("奖品已无可用库存编号")
	ErrShipmentExists     = errors.New("发货单已存在，不能重复发货")
	ErrDuplicateReceipt   = errors.New("物流回执已处理，幂等忽略")
	ErrWinnerIneligible   = errors.New("该账号不在正式名单中")
)

// Service 业务服务。所有跨表写操作都在 immediate 事务内完成。
type Service struct {
	db      *gorm.DB
	nowFunc func() time.Time
}

func New(db *gorm.DB) *Service {
	return &Service{db: db, nowFunc: func() time.Time { return time.Now().UTC() }}
}

// WithClock 仅供测试注入可控时钟。
func (s *Service) WithClock(f func() time.Time) *Service {
	s.nowFunc = f
	return s
}

func (s *Service) now() time.Time { return s.nowFunc() }
