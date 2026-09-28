package goparcellocker

import "time"

// Size 是格口/包裹的尺寸等级。格口可以容纳尺寸不大于自身的包裹。
type Size uint8

const (
	SizeUnknown Size = iota
	SizeSmall
	SizeMedium
	SizeLarge
)

// Valid 报告尺寸是否为已定义的合法值。
func (s Size) Valid() bool { return s >= SizeSmall && s <= SizeLarge }

func (s Size) String() string {
	switch s {
	case SizeSmall:
		return "S"
	case SizeMedium:
		return "M"
	case SizeLarge:
		return "L"
	default:
		return "?"
	}
}

// CompartmentSpec 用于配置一个格口。
type CompartmentSpec struct {
	ID   string
	Size Size
}

// Package 是预约时登记的包裹信息。
type Package struct {
	// TrackingNo 业务侧运单号，仅用于展示与交接记录。
	TrackingNo string
	// Size 包裹所需的最小格口尺寸。
	Size Size
	// ContentHash 调用方对包裹内容计算的摘要；用于区分“重复投递同一内容（幂等）”
	// 与“凭证相同但包裹信息变化（冲突）”。
	ContentHash []byte
}

// CompartmentState 是格口当前的对外状态。
type CompartmentState struct {
	ID    string
	Size  Size
	InUse bool
	// ReservationID/ItemID 仅在占用期间有值。
	ReservationID string
	ItemID        int
}

// ItemStatus 是预约条目（一个包裹一个条目）的生命周期状态。
type ItemStatus uint8

const (
	// ItemReserved 已预约，等待投递。
	ItemReserved ItemStatus = iota + 1
	// ItemDelivered 已投递，等待取件。
	ItemDelivered
	// ItemPickedUp 已取件，终态。
	ItemPickedUp
	// ItemCanceled 预约被取消，终态。
	ItemCanceled
	// ItemExpired 投递超时未投递，或投递后取件超时，终态。
	ItemExpired
)

func (s ItemStatus) String() string {
	switch s {
	case ItemReserved:
		return "reserved"
	case ItemDelivered:
		return "delivered"
	case ItemPickedUp:
		return "picked_up"
	case ItemCanceled:
		return "canceled"
	case ItemExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// ItemView 是预约中单个包裹条目的快照。
type ItemView struct {
	ItemID         int
	TrackingNo     string
	Size           Size
	Status         ItemStatus
	CompartmentID  string
	DeliveredAt    time.Time
	PickupDeadline time.Time
}

// ReservationStatus 是整笔预约的汇总状态。
type ReservationStatus struct {
	ID               string
	CourierID        string
	Items            []ItemView
	ReservedAt       time.Time
	DeliveryDeadline time.Time
	// Active 为 true 表示仍有待投递的条目；任意条目终结（取消/投递超时）
	// 都会让预约不可再投递。
	Active bool
}

// ReserveReceipt 是预约成功的返回：每个包裹对应一条投递凭证。
type ReserveReceipt struct {
	ReservationID    string
	DeliveryDeadline time.Time
	// Items 与请求中的 packages 顺序一一对应。
	Items []ReservedItem
}

// ReservedItem 携带分配结果与只在此时出现一次的投递凭证明文。
type ReservedItem struct {
	ItemID          int
	CompartmentID   string
	Size            Size
	TrackingNo      string
	DeliveryToken   string
	DeliveryExpires time.Time
}

// DeliveryReceipt 是投递成功（或幂等重放）的返回。
type DeliveryReceipt struct {
	ReservationID  string
	ItemID         int
	CompartmentID  string
	TrackingNo     string
	PickupCode     string
	DeliveredAt    time.Time
	PickupDeadline time.Time
	// Replayed 为 true 表示同一投递凭证之前已成功消费过，本次为幂等重放，
	// 返回的 PickupCode 与首次完全一致。
	Replayed bool
}

// PickupReceipt 是取件成功的返回。
type PickupReceipt struct {
	ReservationID string
	ItemID        int
	CompartmentID string
	TrackingNo    string
	PickedUpAt    time.Time
}

// EventKind 标识交接记录的事件类型。
type EventKind uint8

const (
	EventReserved EventKind = iota + 1
	EventDelivered
	EventPickedUp
	EventCanceled
	EventExpired
)

func (k EventKind) String() string {
	switch k {
	case EventReserved:
		return "reserved"
	case EventDelivered:
		return "delivered"
	case EventPickedUp:
		return "picked_up"
	case EventCanceled:
		return "canceled"
	case EventExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// HandoffRecord 是一条交接记录。
//
// 安全约束：记录中只保存凭证的安全摘要（CredentialFingerprint）与短标签
// （CredentialHint，取摘要前若干位，仅供人工对账），任何场景都不会写出
// 投递凭证或取件码明文。
type HandoffRecord struct {
	At                    time.Time
	Kind                  EventKind
	ReservationID         string
	ItemID                int
	CompartmentID         string
	TrackingNo            string
	CourierID             string
	CredentialFingerprint string
	CredentialHint        string
	// Detail 仅记录非敏感细节，例如过期阶段 "delivery" / "pickup"。
	Detail string
}

// ExpiredItem 描述一次过期推进中被终结的条目。
type ExpiredItem struct {
	ReservationID string
	ItemID        int
	CompartmentID string
	// Phase 为 "delivery"（投递超时未投递）或 "pickup"（取件超时）。
	Phase string
}

// StatusSnapshot 是系统整体状态快照。
type StatusSnapshot struct {
	Compartments []CompartmentState
	Reservations []ReservationStatus
	// Free/Used 按尺寸统计格口数量。
	FreeBySize map[Size]int
	UsedBySize map[Size]int
}
