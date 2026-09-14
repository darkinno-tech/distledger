package distledger

import "fmt"

// UserKey 唯一标识一个「外部用户」。
//
// 库不拥有用户表：UserID 由接入方定义，库只把它当作不透明标识符使用。
// 所有键都强制携带 TenantID，这是刻意的——多租户环境下漏带租户条件
// 会导致跨租户数据泄露，把它做成结构体字段可以让编译器帮忙兜住。
type UserKey struct {
	TenantID int64
	UserID   int64
}

// OrderKey 唯一标识一笔外部订单。
type OrderKey struct {
	TenantID int64
	OrderID  string
}

// Validate 校验用户键。
func (k UserKey) Validate() error {
	if k.TenantID < 0 {
		return fieldErrf("tenant_id", "must be >= 0, got %d", k.TenantID)
	}
	if k.UserID <= 0 {
		return fieldErrf("user_id", "must be > 0, got %d", k.UserID)
	}
	return nil
}

// String 返回便于日志排查的形式。
func (k UserKey) String() string {
	return fmt.Sprintf("t%d/u%d", k.TenantID, k.UserID)
}

// Validate 校验订单键。
func (k OrderKey) Validate() error {
	if k.TenantID < 0 {
		return fieldErrf("tenant_id", "must be >= 0, got %d", k.TenantID)
	}
	if k.OrderID == "" {
		return fieldErr("order_id", "must not be empty")
	}
	if len(k.OrderID) > MaxOrderIDLen {
		return fieldErrf("order_id", "must be at most %d bytes, got %d", MaxOrderIDLen, len(k.OrderID))
	}
	return nil
}

// String 返回便于日志排查的形式。
func (k OrderKey) String() string {
	return fmt.Sprintf("t%d/%s", k.TenantID, k.OrderID)
}

// 输入长度上限。它们存在的意义是防止无界输入把幂等键计算与存储拖垮。
const (
	// MaxOrderIDLen 是订单号的最大字节数。
	MaxOrderIDLen = 64
	// MaxOrderItemIDLen 是订单明细号的最大字节数。
	MaxOrderItemIDLen = 128
	// MaxIdemKeyLen 是调用方自定义幂等键的最大字节数。
	MaxIdemKeyLen = 64
	// MaxRemarkLen 是备注字段的最大字节数。
	MaxRemarkLen = 256
)
