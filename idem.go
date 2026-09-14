package distledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// idemKeyVersion 参与幂等键派生。
//
// 它必须随派生规则的变化而递增：一旦改变派生方式，已经在途的事件的幂等性
// 会失效（同一笔业务会被认为是新业务）。把它显式写进哈希输入，是为了让
// 这种变化至少是「有意的、可追溯的」。
const idemKeyVersion = "v1"

// 幂等键的业务命名空间。不同语义的动作使用不同命名空间，避免键碰撞。
//
// 这里刻意只有两个命名空间：入账与「调用方自定义」。结算、状态迁移等动作
// 不靠幂等键去重，而是靠状态机的合法迁移 + 乐观锁版本号——它们本身就是
// 幂等的，不需要额外的键。
const (
	nsAccrue = "accrue"
	nsUser   = "user"
)

// idemKeyLen 是最终幂等键的十六进制长度（32 字符 = 128 位）。
//
// 128 位足够避免碰撞：即使有 10^12 条记录，碰撞概率也在 10^-15 量级。
const idemKeyLen = 32

// hashParts 把若干字段编码成一个无歧义的摘要。
//
// 使用「长度前缀 + 内容」而不是分隔符拼接：如果直接用 "|" 拼接，
// orderID="a|b" 与 orderID="a", itemID="b" 会产生相同的摘要，
// 攻击者可以借此构造幂等键碰撞，让真实的佣金被静默丢弃。
func hashParts(parts ...string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		h.Write(lenBuf[:])
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:idemKeyLen/2])
}

// accrueIdemKey 派生一笔佣金入账的幂等键。
//
// 同一笔订单、同一个明细、同一个分销员、同一层级，永远得到同一个键。
// 这就是「重复投递是 no-op」的全部依据（见 ADR-008）。
//
// override 非空时，调用方提供的键作为**种子**参与派生，而不是直接当作
// 最终键使用：一笔订单会产生多条佣金（每个明细 × 每个层级），如果所有
// 佣金共用同一个键，第二条起就会被判为重复而静默丢弃。
func accrueIdemKey(order OrderKey, itemID string, agentUserID int64, layer int, override string) string {
	if override == "" {
		return hashParts(
			idemKeyVersion,
			nsAccrue,
			i64s(order.TenantID),
			order.OrderID,
			itemID,
			i64s(agentUserID),
			i64s(int64(layer)),
		)
	}
	return hashParts(
		idemKeyVersion,
		nsUser,
		i64s(order.TenantID),
		override,
		order.OrderID,
		itemID,
		i64s(agentUserID),
		i64s(int64(layer)),
	)
}

// validateIdemKeyOverride 校验调用方自定义幂等键。
//
// 限制字符集与长度，避免把不可控的长字符串带进唯一索引与日志。
func validateIdemKeyOverride(raw string) error {
	if raw == "" {
		return fieldErr("idem_key", "must not be empty when provided")
	}
	if len(raw) > MaxIdemKeyLen {
		return fieldErrf("idem_key", "must be at most %d bytes, got %d", MaxIdemKeyLen, len(raw))
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == ':':
		default:
			return fieldErrf("idem_key",
				"contains illegal byte %q; allowed: [A-Za-z0-9-_.:]", string(c))
		}
	}
	return nil
}

func i64s(v int64) string {
	// 手工实现以避免 fmt 在热路径上的反射开销。
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-(v + 1)) + 1
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
