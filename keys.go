package distledger

import "fmt"

// UserKey uniquely identifies an external user.
//
// The library does not own a user table: UserID is defined by the integrator and
// is treated as an opaque identifier. Every key carries a TenantID by design —
// in a multi-tenant environment, dropping the tenant condition leaks data
// across tenants, and making it a struct field lets the compiler help catch
// that.
type UserKey struct {
	TenantID int64
	UserID   int64
}

// OrderKey uniquely identifies an external order.
type OrderKey struct {
	TenantID int64
	OrderID  string
}

// Validate checks the user key.
func (k UserKey) Validate() error {
	if k.TenantID < 0 {
		return fieldErrf("tenant_id", "must be >= 0, got %d", k.TenantID)
	}
	if k.UserID <= 0 {
		return fieldErrf("user_id", "must be > 0, got %d", k.UserID)
	}
	return nil
}

// String returns a form convenient for log triage.
func (k UserKey) String() string {
	return fmt.Sprintf("t%d/u%d", k.TenantID, k.UserID)
}

// Validate checks the order key.
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

// String returns a form convenient for log triage.
func (k OrderKey) String() string {
	return fmt.Sprintf("t%d/%s", k.TenantID, k.OrderID)
}

// Input length limits. They exist to keep unbounded input from overwhelming
// idempotency key computation and storage.
const (
	// MaxOrderIDLen is the maximum length in bytes of an order ID.
	MaxOrderIDLen = 64
	// MaxOrderItemIDLen is the maximum length in bytes of an order item ID.
	MaxOrderItemIDLen = 128
	// MaxIdemKeyLen is the maximum length in bytes of a caller-supplied
	// idempotency key.
	MaxIdemKeyLen = 64
	// MaxRemarkLen is the maximum length in bytes of a remark field.
	MaxRemarkLen = 256
)
