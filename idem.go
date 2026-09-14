package distledger

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// idemKeyVersion participates in idempotency key derivation.
//
// It must be bumped whenever the derivation rules change: once the derivation
// changes, in-flight events lose their idempotency (the same business action is
// then seen as a new one). Writing it explicitly into the hash input makes such
// a change at least deliberate and traceable.
const idemKeyVersion = "v1"

// Business namespaces for idempotency keys. Actions with different semantics use
// different namespaces so that keys cannot collide.
//
// The namespaces are per action: accrual, reversal, refund voucher and
// caller-supplied. What is deliberately absent is a namespace for settlement or
// state transitions - those do not dedupe through an idempotency key but through
// legal state machine transitions plus version CAS, because they are already
// idempotent and need no extra key.
const (
	nsAccrue  = "accrue"
	nsReverse = "reverse"
	nsRefund  = "refund"
	nsUser    = "user"
)

// idemKeyLen is the hex length of the final idempotency key (32 characters =
// 128 bits).
//
// 128 bits is enough to avoid collisions: even with 10^12 records the collision
// probability is on the order of 10^-15.
const idemKeyLen = 32

// hashParts encodes several fields into an unambiguous digest.
//
// It uses length-prefixed contents rather than separator concatenation: with
// plain "|" concatenation, orderID="a|b" and orderID="a", itemID="b" would
// produce the same digest, letting an attacker construct idempotency key
// collisions that silently drop real commissions.
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

// accrueIdemKey derives the idempotency key for one commission accrual.
//
// The same order, the same item, the same agent, and the same level always yield
// the same key. That is the entire basis for "a duplicate delivery is a no-op"
// (see ADR-008).
//
// When override is non-empty, the caller-supplied key participates in the
// derivation as a **seed** rather than being used as the final key: one order
// produces several commission entries (each item x each level), and if all of
// them shared one key, everything from the second entry onward would be judged
// a duplicate and silently dropped.
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

// reverseIdemKey derives the idempotency key for one reversal action.
//
// The key point is that the key encodes the **cumulative value after the
// reversal** (target), not this call's delta. That grounds idempotency in the
// state to be reached rather than the action to be performed:
//
//   - Re-delivering the same refund computes the same target, so the second
//     attempt is stopped by the unique constraint.
//   - When two different refunds happen to land on the same cumulative value,
//     the second one correctly becomes a no-op as well.
//   - When the same refund is delivered concurrently, only one write succeeds
//     and the other gets ErrDuplicate.
//
// With the delta as the key instead, two concurrent calls would each compute the
// same delta, write two records, and debit the money twice.
func reverseIdemKey(order OrderKey, itemID string, agentUserID int64, layer int, target Money) string {
	return hashParts(
		idemKeyVersion,
		nsReverse,
		i64s(order.TenantID),
		order.OrderID,
		itemID,
		i64s(agentUserID),
		i64s(int64(layer)),
		i64s(int64(target)),
	)
}

// refundIdemKey maps one instalment of one caller-supplied refund event to a
// per-item key.
//
// The caller's key identifies the refund event; the item id makes it unique per
// accrual item, because one refund event distributes across several items and
// each gets its own voucher.
func refundIdemKey(order OrderKey, itemID, callerKey string) string {
	return hashParts(idemKeyVersion, nsRefund, i64s(order.TenantID), order.OrderID, itemID, callerKey)
}

// validateIdemKeyOverride validates a caller-supplied idempotency key.
//
// It restricts the character set and the length so that unbounded long strings
// never reach a unique index or the logs.
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
	// Implemented by hand to avoid the reflection overhead of fmt on a hot path.
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
