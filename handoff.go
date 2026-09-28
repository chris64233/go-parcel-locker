package goparcellocker

import (
	"fmt"
	"strings"
	"time"
)

// HandoffEvent 标记一条交接记录对应的生命周期事件。
type HandoffEvent string

// 交接事件类型。
const (
	// EventReserved 预约成功、格口已占用。
	EventReserved HandoffEvent = "reserved"
	// EventDelivered 投递成功，取件凭证已生成。
	EventDelivered HandoffEvent = "delivered"
	// EventPickedUp 取件成功，格口已释放。
	EventPickedUp HandoffEvent = "picked_up"
	// EventReservationCanceled 投递前取消，格口已释放。
	EventReservationCanceled HandoffEvent = "reservation_canceled"
	// EventDeliveryExpired 投递窗口超时，预约作废、格口已释放。
	EventDeliveryExpired HandoffEvent = "delivery_expired"
	// EventPickupExpired 取件窗口超时，格口已释放。
	EventPickupExpired HandoffEvent = "pickup_expired"
	// EventDeliveryRejected 投递被拒（如重放但包裹信息冲突）。
	EventDeliveryRejected HandoffEvent = "delivery_rejected"
)

// HandoffRecord 是一条只追加的交接记录。它只引用凭证的短摘要提示
// （HMAC 摘要前若干字符），任何位置都不保存凭证明文，因此记录可以
// 安全地用于审计与排障。
type HandoffRecord struct {
	Time           time.Time
	Event          HandoffEvent
	ReservationID  string
	CompartmentIDs []string
	// DeliveryHint / PickupHint 是凭证 HMAC 摘要的短前缀，仅用于人工
	// 交叉核对，不能用来还原或猜测凭证。
	DeliveryHint string
	PickupHint   string
	// Reason 记录拒绝类事件的原因（不含任何用户输入的敏感原文）。
	Reason string
}

// hintPrefixLen 是交接记录中保留的摘要前缀长度（十六进制字符数）。
const hintPrefixLen = 12

// Handoffs 返回交接记录的快照副本，按追加顺序排列。
func (l *Locker) Handoffs() []HandoffRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]HandoffRecord, len(l.handoffs))
	for i, r := range l.handoffs {
		r.CompartmentIDs = cloneStrings(r.CompartmentIDs)
		out[i] = r
	}
	return out
}

// shortHint 返回摘要的短前缀用于审计提示；空摘要返回空串。
func shortHint(digest string) string {
	if len(digest) == 0 {
		return ""
	}
	if len(digest) > hintPrefixLen {
		return digest[:hintPrefixLen]
	}
	return digest
}

// String 返回用于审计日志的脱敏单行表示。其中只出现凭证摘要前缀，
// 不可能包含凭证明文或包裹登记内容。
func (r HandoffRecord) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s reservation=%s compartments=%s",
		r.Time.UTC().Format(time.RFC3339), r.Event, r.ReservationID, strings.Join(r.CompartmentIDs, ","))
	if r.DeliveryHint != "" {
		fmt.Fprintf(&b, " delivery=%s…", r.DeliveryHint)
	}
	if r.PickupHint != "" {
		fmt.Fprintf(&b, " pickup=%s…", r.PickupHint)
	}
	if r.Reason != "" {
		fmt.Fprintf(&b, " reason=%q", r.Reason)
	}
	return b.String()
}
