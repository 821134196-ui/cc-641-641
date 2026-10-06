// Package sim 本地模拟直播观众流量与物流事件，不依赖任何外部服务。
package sim

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"lottery/internal/models"
	"lottery/internal/service"
)

type Simulator struct {
	svc  *service.Service
	db   *gorm.DB
	mu   sync.Mutex
	logs []string
}

func New(svc *service.Service) *Simulator {
	return &Simulator{svc: svc, db: svc.DB().DB}
}

func (s *Simulator) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.logs = append(s.logs, line)
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
	s.mu.Unlock()
	log.Println("[sim] " + line)
}

func (s *Simulator) Logs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

// SeedWorld 生成模拟观众与活动期间的电商订单（全部有效，后续可用退货制造复核）。
// 观众账号是全局实体（同一账号可参与不同活动），按 UniqueNo 幂等创建；
// 订单按活动归属，重复播种不会产生重复订单。
func (s *Simulator) SeedWorld(campaignID uint, nAccounts int, ordersEach int) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		created := 0
		for i := 1; i <= nAccounts; i++ {
			uniqueNo := fmt.Sprintf("U%06d", 1000+i)
			var acc models.Account
			err := tx.Where("unique_no = ?", uniqueNo).First(&acc).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				acc = models.Account{Nickname: fmt.Sprintf("观众%02d", i), UniqueNo: uniqueNo}
				if err := tx.Create(&acc).Error; err != nil {
					return err
				}
				created++
			} else if err != nil {
				return err
			}
			var exist int64
			tx.Model(&models.Order{}).Where("campaign_id = ? AND account_id = ?", campaignID, acc.ID).Count(&exist)
			if exist > 0 {
				continue
			}
			for j := 0; j < ordersEach; j++ {
				if err := tx.Create(&models.Order{
					CampaignID: campaignID, AccountID: acc.ID,
					SKU: fmt.Sprintf("SKU-%03d", (i+j)%20), AmountFen: 9900 + j*1000,
					Status: "active",
				}).Error; err != nil {
					return err
				}
			}
		}
		// 再放 3 个没有有效订单的账号：核验阶段应被判定不合格（幂等）。
		for i := 1; i <= 3; i++ {
			uniqueNo := fmt.Sprintf("U%09d", i)
			var acc models.Account
			err := tx.Where("unique_no = ?", uniqueNo).First(&acc).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := tx.Create(&models.Account{
					Nickname: fmt.Sprintf("无订单用户%02d", i), UniqueNo: uniqueNo,
				}).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		}
		s.logf("活动 #%d 模拟数据就绪：新增账号 %d 个（其余复用全局账号）", campaignID, created)
		return nil
	})
}

// RunLiveTraffic 模拟直播进行中的报名流量：随机间隔替观众报名，
// 并穿插重复报名（模拟观众连点/多端提交），重复者必须被唯一索引拒绝。
func (s *Simulator) RunLiveTraffic(campaignID uint, stop <-chan struct{}) {
	var accounts []models.Account
	s.db.Find(&accounts)
	remaining := append([]models.Account(nil), accounts...)
	dupeEvery := 3
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	registered := map[uint]bool{}
	round := 0
	for {
		select {
		case <-stop:
			s.logf("直播流量模拟停止")
			return
		case <-ticker.C:
			round++
			var c models.Campaign
			if err := s.db.First(&c, campaignID).Error; err != nil || c.Status != models.StatusOpen {
				continue
			}
			// 穿插一次重复提交
			if round%dupeEvery == 0 && len(registered) > 0 {
				var dupID uint
				for id := range registered {
					dupID = id
					break
				}
				if _, err := s.svc.Register(campaignID, dupID); err != nil {
					s.logf("账号 %d 重复报名被拒绝：%v（同账号不重复占名额）", dupID, err)
				}
				continue
			}
			if len(remaining) == 0 {
				continue
			}
			idx := (round*7 + 3) % len(remaining)
			acc := remaining[idx]
			remaining = append(remaining[:idx], remaining[idx+1:]...)
			if _, err := s.svc.Register(campaignID, acc.ID); err != nil {
				s.logf("%s 报名失败：%v", acc.Nickname, err)
			} else {
				registered[acc.ID] = true
				s.logf("%s 报名成功（活动 #%d 已报名 %d 人）", acc.Nickname, campaignID, len(registered))
			}
		}
	}
}

// RunLogistics 模拟物流合作方回执：发货 3 秒后推送妥投回执；
// 每 5 个回执额外重发一次历史回执号，用于证明重复回执幂等。
func (s *Simulator) RunLogistics(stop <-chan struct{}) {
	seen := map[string]bool{}
	count := 0
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// 周期性重发一个旧回执
			count++
			if count%5 == 0 && len(seen) > 0 {
				var old string
				for r := range seen {
					old = r
					break
				}
				var oldRec models.LogisticsReceipt
				if err := s.db.Where("receipt_no = ?", old).First(&oldRec).Error; err == nil {
					var sh models.Shipment
					s.db.First(&sh, oldRec.ShipmentID)
					dup, err := s.svc.ReceiveReceipt(old, sh.ShipmentNo, oldRec.Event)
					s.logf("物流方重发回执 %s：duplicate=%v err=%v（发货数量不变）", old, dup, err)
				}
				continue
			}
			// 为最老的一张在途运单推妥投
			var sh models.Shipment
			if err := s.db.Where("status = ?", models.ShipmentShipped).
				Order("shipped_at ASC").First(&sh).Error; err != nil {
				continue
			}
			receiptNo := fmt.Sprintf("RCP-%d-%d", sh.ID, time.Now().UnixNano())
			dup, err := s.svc.ReceiveReceipt(receiptNo, sh.ShipmentNo, "delivered")
			if err != nil {
				s.logf("回执 %s 处理失败：%v", receiptNo, err)
				continue
			}
			seen[receiptNo] = true
			s.logf("运单 %s 收到妥投回执 %s（duplicate=%v）", sh.ShipmentNo, receiptNo, dup)
		}
	}
}
