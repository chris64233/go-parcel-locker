package goparcellocker

import "errors"

// 业务错误：调用方可以用 errors.Is 判定具体原因。
var (
	// ErrInvalidRequest 请求参数不合法（空包裹、重复格口 ID、非法尺寸等）。
	ErrInvalidRequest = errors.New("goparcellocker: invalid request")
	// ErrInvalidDeadline 投递截止时间不晚于当前时间。
	ErrInvalidDeadline = errors.New("goparcellocker: invalid delivery deadline")
	// ErrNoAvailableCompartment 没有足够的“仍可用且尺寸合适”的格口，整笔预约不占用任何格口。
	ErrNoAvailableCompartment = errors.New("goparcellocker: no suitable available compartment")
	// ErrReservationNotFound 预约不存在。
	ErrReservationNotFound = errors.New("goparcellocker: reservation not found")
	// ErrReservationNotActive 预约已投递/已取消/已过期，不能再取消或投递。
	ErrReservationNotActive = errors.New("goparcellocker: reservation is not active")
	// ErrReservationCanceled 预约已取消，投递被拒绝。
	ErrReservationCanceled = errors.New("goparcellocker: reservation is canceled")
	// ErrReservationExpired 预约投递截止时间已过，投递被拒绝。
	ErrReservationExpired = errors.New("goparcellocker: reservation is expired")
	// ErrCredentialInvalid 投递/取件凭证不存在或已失效。
	ErrCredentialInvalid = errors.New("goparcellocker: invalid or unknown credential")
	// ErrCredentialReplayed 凭证是一次性的：重复消费同一凭证且包裹内容与首次不一致。
	ErrCredentialReplayed = errors.New("goparcellocker: credential already consumed with different payload")
	// ErrPackageConflict 首次投递的包裹信息（尺寸/运单号/内容摘要）与预约登记不一致。
	ErrPackageConflict = errors.New("goparcellocker: delivered package conflicts with reservation")
	// ErrPackageNotReady 包裹尚未投递或已被取走，不能取件。
	ErrPackageNotReady = errors.New("goparcellocker: package is not ready for pickup")
	// ErrPackageExpired 包裹超过取件截止时间，已被回收释放。
	ErrPackageExpired = errors.New("goparcellocker: package pickup window expired")
	// ErrCompartmentsInUse 仍有未终结的预约/包裹时，不允许整体重配格口。
	ErrCompartmentsInUse = errors.New("goparcellocker: cannot reconfigure while compartments are in use")
)
