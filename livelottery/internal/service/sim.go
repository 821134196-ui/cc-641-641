package service

import (
	"fmt"
	"time"
)

// DemoResult 一键演示的产物
type DemoResult struct {
	ActivityID int64    `json:"activity_id"`
	Accounts   []string `json:"accounts"`
	PrizeID    int64    `json:"prize_id"`
}

// RunDemo 本地模拟一条完整链路（不依赖任何外部直播/物流系统）：
// 建活动→配奖品→锁定规则库存→批量报名→结束直播→开奖，得到唯一正式名单。
func (s *Service) RunDemo() (*DemoResult, error) {
	drawAt := s.now().Add(1 * time.Hour)
	act, err := s.CreateActivity(CreateActivityInput{
		Name:              "新品首发直播抽奖（演示）",
		DrawAtISO:         drawAt.Format(time.RFC3339),
		MinAccountAgeDays: 7,
		NeedBoundPhone:    true,
		WinnerCount:       5,
		AddressHours:      24,
	})
	if err != nil {
		return nil, err
	}
	prize, err := s.AddPrize(act.ID, "蓝牙耳机", 5)
	if err != nil {
		return nil, err
	}
	if _, err := s.LockActivity(act.ID); err != nil {
		return nil, err
	}

	accounts := []string{"viewer-01", "viewer-02", "viewer-03", "viewer-04",
		"viewer-05", "viewer-06", "viewer-07", "viewer-08"}
	for _, a := range accounts {
		if _, err := s.Register(act.ID, ViewerProfile{
			AccountID: a, OrderNo: "ORD-" + a,
			AccountAgeDays: 30, PhoneBound: true,
		}); err != nil {
			return nil, fmt.Errorf("报名 %s: %w", a, err)
		}
	}
	if _, err := s.EndLive(act.ID); err != nil {
		return nil, err
	}
	if _, err := s.Draw(act.ID); err != nil {
		return nil, err
	}
	return &DemoResult{ActivityID: act.ID, Accounts: accounts, PrizeID: prize.ID}, nil
}
