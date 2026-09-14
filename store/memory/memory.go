// Package memory provides the in-memory implementation of distledger.Store.
//
// It targets the reference and small-deployment use cases: unit tests, demos,
// and single-process installations. Production deployments should use
// store/mysql, which has real row locks and indexes.
//
// # Consistency model
//
//   - Update holds a global write lock, so transactions are fully serialized.
//     The effective isolation level is serialisable.
//   - View holds a read lock and never sees uncommitted writes, because the
//     read and write locks are mutually exclusive.
//   - A failed transaction is undone through an undo log. The cost of a
//     rollback is proportional to the number of objects the transaction
//     touched, not to the size of the data set.
//
// # Why the rollback machinery is this detailed
//
// Most writes are map upserts or tail appends, which undo by restoring the old
// value or truncating to the old length. Two cases are not that simple:
//
//  1. Ordered indexes (settlement due times, account keys) are inserted into
//     and deleted from the middle, which shifts elements. Truncating to the old
//     length there would restore misaligned content.
//  2. Truncating an append-only slice index to zero leaves an empty slice under
//     the key, so a stream of failed transactions would grow the index map
//     without bound.
//
// Hence two undo primitives: append-only indexes record "did the key exist" plus
// the old length, and ordered indexes use a hybrid snapshot - a length for tail
// appends, escalating to a full clone for middle inserts and deletes.
package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/im10furry/distledger"
)

// idemRef is the composite key of the idempotency index.
type idemRef struct {
	tenantID int64
	key      string
}

// dueRef is one entry of the settlement index.
//
// The tenant id is part of the sort key so that a per-tenant query is answered
// by two binary searches instead of a scan over every due entry in the
// installation. Without it, one busy tenant would make every other tenant's
// Maintain call linear in the global backlog, quietly contradicting the
// O(log n + limit) contract the port advertises.
type dueRef struct {
	tenantID int64
	at       time.Time
	id       int64
}

func (r dueRef) less(o dueRef) bool {
	if r.tenantID != o.tenantID {
		return r.tenantID < o.tenantID
	}
	if !r.at.Equal(o.at) {
		return r.at.Before(o.at)
	}
	return r.id < o.id
}

// data holds all state. Every field is accessed only while holding Store.mu.
type data struct {
	agents   map[distledger.UserKey]distledger.Agent
	bindings map[distledger.UserKey]distledger.Binding
	accounts map[distledger.UserKey]distledger.Account

	commissions map[int64]distledger.Commission
	// commissionIDs is an append-only ascending index over commissions.
	commissionIDs []int64
	idem          map[idemRef]int64
	byOrder       map[distledger.OrderKey][]int64
	byAgent       map[distledger.UserKey][]int64

	refunds        map[int64]distledger.Refund
	refundIdem     map[idemRef]int64
	refundsByOrder map[distledger.OrderKey][]int64

	ledger         map[int64]distledger.LedgerEntry
	ledgerIDs      []int64
	ledgerByUser   map[distledger.UserKey][]int64
	ledgerByTenant map[int64][]int64

	// accountIDs is an ordered index over accounts, sorted by (tenant, user).
	//
	// It exists so keyset pagination does not re-sort every account on every
	// page, which turned reconciliation into an O(n^2) walk.
	accountIDs []distledger.UserKey

	// pendingDue holds commission ids whose settlement time is known, sorted by
	// (tenant, at, id).
	//
	// It is a derived index: readers validate freshness and skip stale entries,
	// so drift costs extra scanning but can never produce a wrong number.
	pendingDue []dueRef

	nextID int64
}

func newData() *data {
	return &data{
		agents:         make(map[distledger.UserKey]distledger.Agent),
		bindings:       make(map[distledger.UserKey]distledger.Binding),
		accounts:       make(map[distledger.UserKey]distledger.Account),
		commissions:    make(map[int64]distledger.Commission),
		idem:           make(map[idemRef]int64),
		byOrder:        make(map[distledger.OrderKey][]int64),
		byAgent:        make(map[distledger.UserKey][]int64),
		refunds:        make(map[int64]distledger.Refund),
		refundIdem:     make(map[idemRef]int64),
		refundsByOrder: make(map[distledger.OrderKey][]int64),
		ledger:         make(map[int64]distledger.LedgerEntry),
		ledgerByUser:   make(map[distledger.UserKey][]int64),
		ledgerByTenant: make(map[int64][]int64),
	}
}

// Store is the in-memory implementation of distledger.Store.
// The zero value is not usable; construct it with New.
type Store struct {
	mu     sync.RWMutex
	d      *data
	closed bool
}

// New returns an empty, ready-to-use in-memory Store.
func New() *Store { return &Store{d: newData()} }

// StoreKind implements distledger.ReportedStore.
func (s *Store) StoreKind() string { return "memory" }

// Close releases the Store. It is safe to call repeatedly.
//
// After closing, reads and writes return ErrClosed. The data is retained so it
// can still be inspected.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// errNestedTx reports a transaction opened inside another transaction.
//
// The in-memory implementation uses a single mutex, so a nested transaction
// would deadlock. Returning an error is far better than handing the caller a
// hung process: a deadlock in production is nearly impossible to diagnose.
var errNestedTx = errors.New("distledger/memory: nested transaction is not supported")

type txMarkerKey struct{}

func inTx(ctx context.Context) bool { return ctx.Value(txMarkerKey{}) != nil }

// View runs fn in a read-only transaction.
func (s *Store) View(ctx context.Context, fn func(ctx context.Context, r distledger.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inTx(ctx) {
		return errNestedTx
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return distledger.ErrClosed
	}
	return fn(context.WithValue(ctx, txMarkerKey{}, true), &reader{d: s.d})
}

// Update runs fn in a read-write transaction.
//
// If fn returns an error, every write it already performed is undone and the
// data returns to its pre-transaction state.
func (s *Store) Update(ctx context.Context, fn func(ctx context.Context, tx distledger.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if inTx(ctx) {
		return errNestedTx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return distledger.ErrClosed
	}

	u := newUndo(s.d)
	tx := &tx{reader: reader{d: s.d}, u: u}
	if err := fn(context.WithValue(ctx, txMarkerKey{}, true), tx); err != nil {
		u.rollback(s.d)
		return err
	}
	return nil
}

// ── Undo log ────────────────────────────────────────────────────────────
//
// The in-memory store mutates the real maps and keeps an undo log rather than
// staging writes in a journal. Readers therefore get read-your-writes for free,
// and no other transaction can observe uncommitted data because the write lock
// is held for the whole transaction.

type undoEntry[V any] struct {
	val     V
	existed bool
}

// appendIndex is the undo record for an append-only slice index.
type appendIndex struct {
	length  int
	existed bool
}

// orderedUndo is the undo record for an index that may change in the middle.
type orderedUndo[T any] struct {
	saved bool
	// full is non-nil once a structural change forced a full snapshot; the
	// slice is then restored wholesale.
	full []T
	// length is the pre-transaction length, valid only while full is nil.
	length int
}

// noteAppend records a tail append. This is the cheap path.
func (u *orderedUndo[T]) noteAppend(cur []T) {
	if u.saved {
		return
	}
	u.saved = true
	u.length = len(cur)
}

// noteStructural records a middle insert or a delete, upgrading to a full
// snapshot.
//
// The critical detail: if a length snapshot was already taken, everything up to
// this point was a tail append, so the pre-transaction content is exactly
// cur[:length]. Cloning the whole of cur would treat entries appended by this
// transaction as if they had been there beforehand, and the rollback would
// leave them behind.
func (u *orderedUndo[T]) noteStructural(cur []T) {
	if u.full != nil {
		return
	}
	if u.saved {
		u.full = slices.Clone(cur[:u.length])
	} else {
		u.full = slices.Clone(cur)
	}
	u.saved = true
}

// restore returns the slice as it was before the transaction.
func (u *orderedUndo[T]) restore(cur []T) []T {
	if !u.saved {
		return cur
	}
	if u.full != nil {
		return u.full
	}
	return cur[:u.length]
}

// undo records the pre-transaction value of everything a transaction touches.
type undo struct {
	agents      map[distledger.UserKey]undoEntry[distledger.Agent]
	bindings    map[distledger.UserKey]undoEntry[distledger.Binding]
	accounts    map[distledger.UserKey]undoEntry[distledger.Account]
	commissions map[int64]undoEntry[distledger.Commission]
	ledger      map[int64]undoEntry[distledger.LedgerEntry]
	idem        map[idemRef]undoEntry[int64]
	refunds     map[int64]undoEntry[distledger.Refund]
	refundIdem  map[idemRef]undoEntry[int64]

	// Append-only indexes: old length plus whether the key existed at all.
	byOrder        map[distledger.OrderKey]appendIndex
	byAgent        map[distledger.UserKey]appendIndex
	refundsByOrder map[distledger.OrderKey]appendIndex
	ledgerByUser   map[distledger.UserKey]appendIndex
	ledgerByTenant map[int64]appendIndex
	// The two global id slices always exist, so they only need a length.
	ledgerIDsLen     int
	commissionIDsLen int

	// Ordered indexes: hybrid snapshots.
	due        orderedUndo[dueRef]
	accountIDs orderedUndo[distledger.UserKey]

	nextID int64
}

func newUndo(d *data) *undo {
	return &undo{
		agents:           make(map[distledger.UserKey]undoEntry[distledger.Agent]),
		bindings:         make(map[distledger.UserKey]undoEntry[distledger.Binding]),
		accounts:         make(map[distledger.UserKey]undoEntry[distledger.Account]),
		commissions:      make(map[int64]undoEntry[distledger.Commission]),
		ledger:           make(map[int64]undoEntry[distledger.LedgerEntry]),
		idem:             make(map[idemRef]undoEntry[int64]),
		refunds:          make(map[int64]undoEntry[distledger.Refund]),
		refundIdem:       make(map[idemRef]undoEntry[int64]),
		byOrder:          make(map[distledger.OrderKey]appendIndex),
		byAgent:          make(map[distledger.UserKey]appendIndex),
		refundsByOrder:   make(map[distledger.OrderKey]appendIndex),
		ledgerByUser:     make(map[distledger.UserKey]appendIndex),
		ledgerByTenant:   make(map[int64]appendIndex),
		ledgerIDsLen:     len(d.ledgerIDs),
		commissionIDsLen: len(d.commissionIDs),
		nextID:           d.nextID,
	}
}

func saveAgent(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.agents[k]; ok {
		return
	}
	v, existed := d.agents[k]
	u.agents[k] = undoEntry[distledger.Agent]{val: v, existed: existed}
}

func saveBinding(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.bindings[k]; ok {
		return
	}
	v, existed := d.bindings[k]
	u.bindings[k] = undoEntry[distledger.Binding]{val: v, existed: existed}
}

func saveAccount(u *undo, d *data, k distledger.UserKey) {
	if _, ok := u.accounts[k]; ok {
		return
	}
	v, existed := d.accounts[k]
	u.accounts[k] = undoEntry[distledger.Account]{val: v, existed: existed}
}

func saveCommission(u *undo, d *data, id int64) {
	if _, ok := u.commissions[id]; ok {
		return
	}
	v, existed := d.commissions[id]
	u.commissions[id] = undoEntry[distledger.Commission]{val: v, existed: existed}
}

func saveLedger(u *undo, d *data, id int64) {
	if _, ok := u.ledger[id]; ok {
		return
	}
	v, existed := d.ledger[id]
	u.ledger[id] = undoEntry[distledger.LedgerEntry]{val: v, existed: existed}
}

func saveRefund(u *undo, d *data, id int64) {
	if _, ok := u.refunds[id]; ok {
		return
	}
	v, existed := d.refunds[id]
	u.refunds[id] = undoEntry[distledger.Refund]{val: v, existed: existed}
}

func saveRefundIdem(u *undo, d *data, k idemRef) {
	if _, ok := u.refundIdem[k]; ok {
		return
	}
	v, existed := d.refundIdem[k]
	u.refundIdem[k] = undoEntry[int64]{val: v, existed: existed}
}

func saveIdem(u *undo, d *data, k idemRef) {
	if _, ok := u.idem[k]; ok {
		return
	}
	v, existed := d.idem[k]
	u.idem[k] = undoEntry[int64]{val: v, existed: existed}
}

// noteAppendIndex records the state of an append-only index before first use.
func noteAppendIndex[K comparable, V any](u map[K]appendIndex, m map[K][]V, k K) {
	if _, ok := u[k]; ok {
		return
	}
	_, existed := m[k]
	u[k] = appendIndex{length: len(m[k]), existed: existed}
}

// restoreAppendIndex undoes every recorded append-only index change.
//
// Keys the transaction created are deleted rather than left behind as empty
// slices: an empty slice is invisible to readers but keeps the map entry alive
// forever, so a stream of failed transactions would grow the index without
// bound.
func restoreAppendIndex[K comparable, V any](u map[K]appendIndex, m map[K][]V) {
	for k, s := range u {
		if !s.existed {
			delete(m, k)
			continue
		}
		m[k] = m[k][:s.length]
	}
}

// rollback restores data to its pre-transaction state.
func (u *undo) rollback(d *data) {
	restoreMap(u.agents, d.agents)
	restoreMap(u.bindings, d.bindings)
	restoreMap(u.accounts, d.accounts)
	restoreMap(u.commissions, d.commissions)
	restoreMap(u.ledger, d.ledger)
	restoreMap(u.idem, d.idem)
	restoreMap(u.refunds, d.refunds)
	restoreMap(u.refundIdem, d.refundIdem)

	restoreAppendIndex(u.byOrder, d.byOrder)
	restoreAppendIndex(u.byAgent, d.byAgent)
	restoreAppendIndex(u.refundsByOrder, d.refundsByOrder)
	restoreAppendIndex(u.ledgerByUser, d.ledgerByUser)
	restoreAppendIndex(u.ledgerByTenant, d.ledgerByTenant)
	d.ledgerIDs = d.ledgerIDs[:u.ledgerIDsLen]
	d.commissionIDs = d.commissionIDs[:u.commissionIDsLen]

	d.pendingDue = u.due.restore(d.pendingDue)
	d.accountIDs = u.accountIDs.restore(d.accountIDs)
	d.nextID = u.nextID
}

func restoreMap[K comparable, V any](src map[K]undoEntry[V], dst map[K]V) {
	for k, e := range src {
		if e.existed {
			dst[k] = e.val
		} else {
			delete(dst, k)
		}
	}
}
