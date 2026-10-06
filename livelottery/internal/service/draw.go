package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"livelottery/internal/models"
)

// DrawResult 开奖输出
type DrawResult struct {
	Batch   models.DrawBatch      `json:"batch"`
	Winners []models.WinnerRecord `json:"winners"`
}

// Draw 执行开奖并公布唯一正式名单。
//
// 核心保证：
//  1. 仅 ended 可开奖；
//  2. draw_batches.activity_id 唯一索引 —— 数据库层拒绝第二份正式名单，
//     即使两个请求并发也只有一个事务能插入成功；
//  3. 使用锁定快照里的规则（人数、算法），可依据冻结的合格名单离线复算；
//  4. 确定性洗牌：固定种子 + 有序输入，任何人可复核结果哈希。
func (s *Service) Draw(activityID int64) (*DrawResult, error) {
	out := &DrawResult{}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		act, err := getActivity(tx, activityID)
		if err != nil {
			return err
		}
		if act.Status != models.StatusEnded {
			if act.Status == models.StatusDrawn {
				return ErrAlreadyDrawn
			}
			return ErrInvalidState
		}
		rule, err := act.Rule()
		if err != nil {
			return err
		}

		// 开奖时核验资格：仅 registered/eligible 进入候选；复核中/失效剔除
		var candidates []models.Registration
		if err := tx.Where("activity_id = ? AND status IN ?", activityID,
			[]string{models.RegRegistered, models.RegEligible}).
			Find(&candidates).Error; err != nil {
			return err
		}
		// 按账号排序，保证输入确定
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].AccountID < candidates[j].AccountID })

		ids := make([]int64, 0, len(candidates))
		accounts := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.ID)
			accounts = append(accounts, c.AccountID)
		}

		count := rule.WinnerCount
		if count > len(accounts) {
			count = len(accounts) // 合格人数不足则按实际人数开奖
		}

		// 固定种子：活动ID + 锁定时刻 + 合格名单，开奖结果可复算
		lockedAt := ""
		if act.LockedAt != nil {
			lockedAt = act.LockedAt.UTC().Format(time.RFC3339Nano)
		}
		seedBase := fmt.Sprintf("%d|%s|%s", act.ID, lockedAt, strings.Join(accounts, ","))
		seedSum := sha256.Sum256([]byte(seedBase))
		seed := int64(0)
		for _, b := range seedSum[:8] {
			seed = (seed << 8) | int64(b)
		}

		shuffled := make([]string, len(accounts))
		copy(shuffled, accounts)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		winners := shuffled[:count]

		eligJSON, _ := json.Marshal(ids)
		hash := hashWinners(winners)
		batch := models.DrawBatch{
			ActivityID:      activityID,
			BatchCode:       fmt.Sprintf("DRAW-%d-%s", activityID, hex.EncodeToString(seedSum[:6])),
			Seed:            seed,
			Algorithm:       rule.Algorithm,
			EligibleIDsJSON: string(eligJSON),
			ResultHash:      hash,
		}
		// 唯一索引在这里拒绝重复开奖（并发亦然）
		if err := tx.Create(&batch).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyDrawn
			}
			return err
		}

		prizeID := s.firstPrizeID(tx, activityID)
		records := make([]models.WinnerRecord, 0, count)
		for i, acc := range winners {
			records = append(records, models.WinnerRecord{
				DrawBatchID: batch.ID,
				ActivityID:  activityID,
				AccountID:   acc,
				Rank:        i + 1,
				PrizeID:     prizeID,
				Status:      models.WinPending,
			})
		}
		if err := tx.CreateInBatches(records, 500).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyDrawn
			}
			return err
		}

		now := s.now()
		act.Status = models.StatusDrawn
		act.DrawnAt = &now
		if err := tx.Save(act).Error; err != nil {
			return err
		}
		auditTx(tx, activityID, "operator", "draw.publish",
			fmt.Sprintf("公布正式名单 %d 人 hash=%s", count, hash))

		out.Batch = batch
		out.Winners = records
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetOfficialResult 读取唯一正式名单
func (s *Service) GetOfficialResult(activityID int64) (*DrawResult, error) {
	var batch models.DrawBatch
	if err := s.db.Where("activity_id = ?", activityID).First(&batch).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, ErrNotDrawn
		}
		return nil, err
	}
	var winners []models.WinnerRecord
	if err := s.db.Where("draw_batch_id = ?", batch.ID).Order("rank asc").Find(&winners).Error; err != nil {
		return nil, err
	}
	return &DrawResult{Batch: batch, Winners: winners}, nil
}

// VerifyResultHash 用当前存储的名单复算哈希，与批次冻结哈希比对（审计）
func (s *Service) VerifyResultHash(activityID int64) (bool, string, error) {
	res, err := s.GetOfficialResult(activityID)
	if err != nil {
		return false, "", err
	}
	accs := make([]string, 0, len(res.Winners))
	for _, w := range res.Winners {
		accs = append(accs, w.AccountID)
	}
	got := hashWinners(accs)
	return got == res.Batch.ResultHash, got, nil
}

func hashWinners(ordered []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\n")))
	return hex.EncodeToString(sum[:])
}
