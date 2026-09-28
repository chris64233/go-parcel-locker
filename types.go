package goparcellocker

import "errors"

// Size 是格口/包裹的尺寸等级。数值越大代表尺寸越大；格口尺寸 >= 包裹尺寸
// 即视为可以容纳。
type Size int

// 预定义的尺寸等级。允许使用任意有序整数扩展自定义等级。
const (
	SizeSmall  Size = 1
	SizeMedium Size = 2
	SizeLarge  Size = 3
	SizeXLarge Size = 4
)

// Valid 报告尺寸是否为正数。
func (s Size) Valid() bool { return s > 0 }

// 预约的生命周期状态。
//
//   - reserved：已预约，等待快递员投递
//   - delivered：已投递，等待收件人取件
//   - pickedUp：已取件（终态）
//   - canceled：预约在投递前被取消（终态）
//   - expired：投递超时或取件超时（终态）
type ReservationState string

const (
	StateReserved  ReservationState = "reserved"
	StateDelivered ReservationState = "delivered"
	StatePickedUp  ReservationState = "picked_up"
	StateCanceled  ReservationState = "canceled"
	StateExpired   ReservationState = "expired"
)

// Terminal 报告该状态是否为终态。
func (s ReservationState) Terminal() bool {
	switch s {
	case StatePickedUp, StateCanceled, StateExpired:
		return true
	default:
		return false
	}
}

// 错误哨兵。调用方应使用 errors.Is 判定。
var (
	// ErrNotFound 表示格口或预约不存在。
	ErrNotFound = errors.New("locker: not found")
	// ErrInvalidRequest 表示请求参数非法（尺寸、数量、包裹信息等）。
	ErrInvalidRequest = errors.New("locker: invalid request")
	// ErrInsufficientCapacity 表示没有足够的合适且可用的格口完成整笔预约。
	ErrInsufficientCapacity = errors.New("locker: insufficient compartment capacity")
	// ErrCredentialInvalid 表示凭证明文错误（查无此凭证）。
	ErrCredentialInvalid = errors.New("locker: invalid credential")
	// ErrCredentialExpired 表示凭证对应的预约已超时。
	ErrCredentialExpired = errors.New("locker: credential expired")
	// ErrCredentialConsumed 表示投递凭证已经成功消费过。
	ErrCredentialConsumed = errors.New("locker: delivery credential already consumed")
	// ErrAlreadyPickedUp 表示该取件凭证已经被成功使用过。
	ErrAlreadyPickedUp = errors.New("locker: parcel already picked up")
	// ErrReservationCanceled 表示预约已被取消，不能再投递。
	ErrReservationCanceled = errors.New("locker: reservation canceled")
	// ErrConflict 表示同一投递凭证重复使用但包裹信息与首次不一致，
	// 或对已投递/已取件的预约执行取消等冲突操作。
	ErrConflict = errors.New("locker: conflicting request")
)
