// Package lottery 实现可复现、可审计的开奖算法。
// 正式开奖使用持久化的随机种子做 Fisher-Yates 洗牌；
// 任何人拿到种子与有序的合格账号ID列表，都能重放出完全相同的中奖名单。
package lottery

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

// NewSeed 生成开奖种子（持久化到 DrawBatch，随正式名单一起公布）。
func NewSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// seededInt 以 sha256(seed || ":" || round) 派生出 [0, mod) 的确定性整数。
func seededInt(seed string, round, mod int) int {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", seed, round)))
	n := new(big.Int).SetBytes(h[:])
	return int(new(big.Int).Mod(n, big.NewInt(int64(mod))).Int64())
}

// SeededShuffle 使用种子对 ids 做确定性 Fisher-Yates 洗牌（原地排序副本）。
func SeededShuffle(seed string, ids []int) []int {
	out := append([]int(nil), ids...)
	for i := len(out) - 1; i > 0; i-- {
		j := seededInt(seed, len(out)-1-i, i+1)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// MerkleRoot 计算有序ID列表的防篡改根哈希（相邻两两哈希，奇数重复末尾）。
func MerkleRoot(ids []int) string {
	if len(ids) == 0 {
		h := sha256.Sum256(nil)
		return hex.EncodeToString(h[:])
	}
	layer := make([]string, len(ids))
	for i, id := range ids {
		layer[i] = fmt.Sprintf("%d", id)
	}
	for len(layer) > 1 {
		var next []string
		for i := 0; i < len(layer); i += 2 {
			a := layer[i]
			b := a
			if i+1 < len(layer) {
				b = layer[i+1]
			}
			h := sha256.Sum256([]byte(a + "|" + b))
			next = append(next, hex.EncodeToString(h[:]))
		}
		layer = next
	}
	return layer[0]
}
