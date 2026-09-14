package distledger

import (
	"errors"
	"fmt"
)

// 哨兵错误。调用方应始终使用 errors.Is 判断，而不要比较错误字符串。
//
// 设计原则：错误里只包含调用方提供的输入信息，不泄露内部实现细节或
// 其他租户/用户的数据（见 PRD §15 安全考量）。
var (
	// ErrInvalidArgument 表示调用方传入的参数不合法（缺少必填字段、数值越界等）。
	ErrInvalidArgument = errors.New("distledger: invalid argument")

	// ErrNotFound 表示查询的对象不存在。
	ErrNotFound = errors.New("distledger: not found")

	// ErrDuplicate 表示幂等键冲突：同一笔账务动作已经入账过。
	//
	// 这不是错误状态——它是幂等保证生效的标志。OnOrderPaid 等入口会把
	// 重复投递转换为「成功且无副作用」的正常返回。
	ErrDuplicate = errors.New("distledger: duplicate idempotency key")

	// ErrIllegalTransition 表示一次非法的状态迁移。
	//
	// 库绝不会「猜测」意图：非法迁移一律报错，不做任何兜底修正。
	ErrIllegalTransition = errors.New("distledger: illegal state transition")

	// ErrConflict 表示乐观锁版本冲突，即对象在读取之后被并发修改。
	ErrConflict = errors.New("distledger: concurrent modification")

	// ErrCycle 表示父子关系构成了环。
	ErrCycle = errors.New("distledger: relation cycle detected")

	// ErrDepthExceeded 表示层级深度超过配置上限。
	ErrDepthExceeded = errors.New("distledger: relation depth exceeded")

	// ErrParentAlreadySet 表示分销员的上级已经设置过，且与本次请求不同。
	//
	// 这是刻意的保护：擅自改上级会导致整个下级树的归属被悄悄重写。
	ErrParentAlreadySet = errors.New("distledger: parent already set")

	// ErrInvalidConfig 表示配置不自洽。
	ErrInvalidConfig = errors.New("distledger: invalid config")

	// ErrOverflow 表示金额运算溢出。出现即代表输入规模超出设计边界。
	ErrOverflow = errors.New("distledger: amount overflow")

	// ErrClosed 表示 Ledger 已经被关闭。
	ErrClosed = errors.New("distledger: ledger closed")

	// ErrBindingLocked 表示买家已经绑定到另一个分销员，且该绑定在归因时刻
	// 仍然有效。
	//
	// 它的存在是为了防抢客：没有这道闸门，任何人都可以在看到订单之后
	// 把自己绑成买家的推广人。BindBuyer 在返回该错误的同时**会返回现有
	// 的绑定**，调用方可以直接使用。
	ErrBindingLocked = errors.New("distledger: buyer already bound to another agent")
)

// FieldError 描述某个字段的校验失败，并包装 ErrInvalidArgument。
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("distledger: invalid %s: %s", e.Field, e.Reason)
}

// Unwrap 使 errors.Is(err, ErrInvalidArgument) 成立。
func (e *FieldError) Unwrap() error { return ErrInvalidArgument }

func fieldErr(field, reason string) error {
	return &FieldError{Field: field, Reason: reason}
}

func fieldErrf(field, format string, args ...any) error {
	return &FieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// TransitionError 描述一次非法的状态迁移，并包装 ErrIllegalTransition。
type TransitionError struct {
	Kind string
	From string
	To   string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("distledger: illegal %s transition: %s -> %s", e.Kind, e.From, e.To)
}

// Unwrap 使 errors.Is(err, ErrIllegalTransition) 成立。
func (e *TransitionError) Unwrap() error { return ErrIllegalTransition }

// ConflictError 描述乐观锁冲突，并包装 ErrConflict。
type ConflictError struct {
	Kind     string
	ID       any
	Expected int64
	Actual   int64
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("distledger: %s %v modified concurrently (expected version %d, actual %d)",
		e.Kind, e.ID, e.Expected, e.Actual)
}

// Unwrap 使 errors.Is(err, ErrConflict) 成立。
func (e *ConflictError) Unwrap() error { return ErrConflict }
