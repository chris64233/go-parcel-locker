package goparcellocker

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// 默认有效期：投递凭证 10 分钟，取件凭证 24 小时。
const (
	DefaultDeliveryTTL = 10 * time.Minute
	DefaultPickupTTL   = 24 * time.Hour
)

// CompartmentConfig 描述一个格口的静态配置。
type CompartmentConfig struct {
	ID   string
	Size Size
}

// ParcelInfo 描述投递时登记的包裹信息。
type ParcelInfo struct {
	TrackingNumber string // 运单号，必填
	Size           Size   // 包裹尺寸，必须与预约尺寸一致
	WeightGrams    int    // 包裹重量（克）
	Sender         string // 寄件人
	Recipient      string // 收件人
}

// ReservationRequest 是预约请求：申请 count 个不小于 Size 的格口。
type ReservationRequest struct {
	Size  Size // 包裹需要的尺寸等级
	Count int  // 需要的格口数量，0 按 1 处理
}

// ReservationReceipt 是预约成功的返回结果。
type ReservationReceipt struct {
	ReservationID  string
	CompartmentIDs []string
	DeliveryToken  string // 投递凭证明文，仅此一次返回
	ExpiresAt      time.Time
}

// DeliveryRequest 是投递请求：凭投递凭证登记包裹。
type DeliveryRequest struct {
	DeliveryToken string
	Parcel        ParcelInfo
}

// DeliveryReceipt 是投递成功的返回结果。
type DeliveryReceipt struct {
	ReservationID   string
	CompartmentIDs  []string
	PickupToken     string // 取件凭证明文
	PickupExpiresAt time.Time
	Parcel          ParcelInfo
}

// PickupReceipt 是取件成功的返回结果。
type PickupReceipt struct {
	ReservationID  string
	CompartmentIDs []string
	Parcel         ParcelInfo
	PickedUpAt     time.Time
}

// CancelReceipt 是取消预约的返回结果。
type CancelReceipt struct {
	ReservationID  string
	CompartmentIDs []string
	CanceledAt     time.Time
}

// SweepResult 汇总一次超时推进释放的内容。
type SweepResult struct {
	// ExpiredReservations 是本次从活跃状态转为过期的预约 ID。
	ExpiredReservations []string
	// ReleasedCompartments 是本次因此变为可用的格口 ID。
	ReleasedCompartments []string
}

// compartment 是格口的运行时状态。
type compartment struct {
	id       string
	size     Size
	occupied bool // 是否正被某个预约占用
	// occupantID 是当前占用者的预约 ID（fencing token）。占用与释放都
	// 必须带预约身份校验：即便出现迟到的旧交接请求，也不可能释放后来
	// 复用该格口的新预约。空闲时为空。
	occupantID string
}

// reservation 是预约的运行时状态。凭证明文绝不落结构，只存摘要。
type reservation struct {
	id             string
	size           Size
	compartmentIDs []string
	state          ReservationState

	deliveryDigest string // 投递凭证摘要
	deliveryExpiry time.Time

	pickupDigest string // 取件凭证摘要（投递后生成）
	pickupExpiry time.Time

	parcel       *ParcelInfo // 投递登记的包裹信息
	parcelDigest parcelDigest

	createdAt time.Time
	updatedAt time.Time
}

// Locker 是并发安全的快递柜。一个实例内的格口与预约互斥访问，
// 所有状态变更都在同一把锁下完成，从而保证预约原子性、投递一次性
// 消费以及格口“精确一次”释放。
type Locker struct {
	mu sync.RWMutex

	clock        Clock
	deliveryTTL  time.Duration
	pickupTTL    time.Duration
	digestSecret digestKey

	compartments map[string]*compartment
	// reservations 保存全部预约（含终态），供查询与审计。
	reservations map[string]*reservation
	// byDelivery / byPickup 只索引活跃凭证：
	// 取件成功或预约取消/过期后对应条目删除，使旧凭证无法再影响
	// 后来复用同一格口的包裹。
	byDelivery map[string]string // 投递凭证摘要 -> 预约 ID
	byPickup   map[string]string // 取件凭证摘要 -> 预约 ID

	// handoffs 是只追加的交接记录，不保存任何凭证明文。
	handoffs []HandoffRecord
}

// Option 配置新创建的 [Locker]。
type Option func(*Locker)

// WithClock 注入时钟（主要用于测试）。
func WithClock(c Clock) Option {
	return func(l *Locker) { l.clock = c }
}

// WithDeliveryTTL 设置投递凭证有效期。
func WithDeliveryTTL(d time.Duration) Option {
	return func(l *Locker) { l.deliveryTTL = d }
}

// WithPickupTTL 设置取件凭证有效期。
func WithPickupTTL(d time.Duration) Option {
	return func(l *Locker) { l.pickupTTL = d }
}

// NewLocker 创建空快递柜，随后用 [Locker.AddCompartments] 配置格口。
func NewLocker(opts ...Option) (*Locker, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	l := &Locker{
		clock:        systemClock{},
		deliveryTTL:  DefaultDeliveryTTL,
		pickupTTL:    DefaultPickupTTL,
		digestSecret: digestKey(secret),
		compartments: make(map[string]*compartment),
		reservations: make(map[string]*reservation),
		byDelivery:   make(map[string]string),
		byPickup:     make(map[string]string),
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.deliveryTTL <= 0 || l.pickupTTL <= 0 {
		return nil, ErrInvalidRequest
	}
	return l, nil
}

// AddCompartments 向快递柜追加一批格口配置。重复 ID 或非法尺寸会返回
// [ErrInvalidRequest]，且本次调用不产生任何部分添加（整笔生效）。
func (l *Locker) AddCompartments(configs ...CompartmentConfig) error {
	if len(configs) == 0 {
		return ErrInvalidRequest
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// 先校验，后修改：任何一项非法都不提前改动状态。
	seen := make(map[string]bool, len(configs))
	for _, c := range configs {
		if c.ID == "" || !c.Size.Valid() {
			return ErrInvalidRequest
		}
		if seen[c.ID] || l.compartments[c.ID] != nil {
			return ErrInvalidRequest
		}
		seen[c.ID] = true
	}
	for _, c := range configs {
		l.compartments[c.ID] = &compartment{id: c.ID, size: c.Size}
	}
	return nil
}

// Reserve 按尺寸整笔申请格口。只有当当前存在 count 个仍可用且尺寸合适的
// 格口时预约才整体成功；成功后这些格口立即被占用，并返回短期有效的投递
// 凭证。并发申请之间不会把同一格口分给两个包裹；容量不足时不占用任何格口。
func (l *Locker) Reserve(req ReservationRequest) (*ReservationReceipt, error) {
	count := req.Count
	if count == 0 {
		count = 1
	}
	if !req.Size.Valid() || count < 0 {
		return nil, ErrInvalidRequest
	}

	token, err := newToken(deliveryPrefix, 32)
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.expireLocked(now)

	// best-fit：选择满足尺寸的最小格口，避免大包裹占走小格口，
	// 也让分配在同等容量下确定、可预期。
	candidates := make([]*compartment, 0)
	for _, c := range l.compartments {
		if !c.occupied && c.size >= req.Size {
			candidates = append(candidates, c)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size != candidates[j].size {
			return candidates[i].size < candidates[j].size
		}
		return candidates[i].id < candidates[j].id
	})
	if len(candidates) < count {
		// 预约失败：不修改任何格口状态。
		return nil, ErrInsufficientCapacity
	}

	id := newReservationID()
	ids := make([]string, 0, count)
	for _, c := range candidates[:count] {
		c.occupied = true
		c.occupantID = id
		ids = append(ids, c.id)
	}

	digest := l.digestSecret.tokenDigest(token)
	expiry := now.Add(l.deliveryTTL)
	r := &reservation{
		id:             id,
		size:           req.Size,
		compartmentIDs: ids,
		state:          StateReserved,
		deliveryDigest: digest,
		deliveryExpiry: expiry,
		createdAt:      now,
		updatedAt:      now,
	}
	l.reservations[id] = r
	l.byDelivery[digest] = id

	l.recordLocked(HandoffRecord{
		Time:           now,
		Event:          EventReserved,
		ReservationID:  id,
		CompartmentIDs: cloneStrings(ids),
		DeliveryHint:   shortHint(digest),
	})

	return &ReservationReceipt{
		ReservationID:  id,
		CompartmentIDs: cloneStrings(ids),
		DeliveryToken:  token,
		ExpiresAt:      expiry,
	}, nil
}

// Deliver 使用投递凭证登记包裹。投递凭证只能成功消费一次；投递成功后生成
// 新的一次性取件凭证。使用同一投递凭证重复投递：
//   - 包裹信息与首次一致：返回原结果（幂等重放，取件凭证相同）；
//   - 包裹信息发生变化：返回包装了 [ErrConflict] 的错误，明确报冲突。
//
// 已取消或已过期的预约不能再投递。
func (l *Locker) Deliver(req DeliveryRequest) (*DeliveryReceipt, error) {
	if req.DeliveryToken == "" {
		return nil, ErrCredentialInvalid
	}
	if err := validateParcel(req.Parcel); err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.expireLocked(now)

	digest := l.digestSecret.tokenDigest(req.DeliveryToken)
	id, ok := l.byDelivery[digest]
	if !ok {
		// 索引中没有：凭证错误，或对应的预约已取消/过期/取件终结。
		if r := l.findByDeliveryDigestLocked(digest); r != nil {
			return nil, stateActionError(r.state, true)
		}
		return nil, ErrCredentialInvalid
	}
	r := l.reservations[id]

	switch r.state {
	case StateReserved:
		if req.Parcel.Size != r.size {
			l.denyLocked(now, EventDeliveryRejected, r, digest, "", "parcel size does not match reservation")
			return nil, ErrInvalidRequest
		}
		if now.After(r.deliveryExpiry) {
			// 理论上 expireLocked 已处理，这里再做一次防御。
			l.expireReservationLocked(r, now)
			return nil, ErrCredentialExpired
		}

		info := req.Parcel
		r.parcel = &info
		r.parcelDigest = digestParcel(req.Parcel, l.digestSecret)
		r.state = StateDelivered
		r.pickupExpiry = now.Add(l.pickupTTL)
		r.updatedAt = now

		pickup := pickupToken(req.DeliveryToken, l.digestSecret)
		pDigest := l.digestSecret.tokenDigest(pickup)
		r.pickupDigest = pDigest
		l.byPickup[pDigest] = r.id

		l.recordLocked(HandoffRecord{
			Time:           now,
			Event:          EventDelivered,
			ReservationID:  r.id,
			CompartmentIDs: cloneStrings(r.compartmentIDs),
			DeliveryHint:   shortHint(digest),
			PickupHint:     shortHint(pDigest),
		})

		return l.deliveryReceipt(r, pickup), nil

	case StateDelivered:
		// 幂等重放 / 冲突判定。
		if digestParcel(req.Parcel, l.digestSecret) != r.parcelDigest {
			l.denyLocked(now, EventDeliveryRejected, r, digest, r.pickupDigest, "parcel info conflict on replay")
			return nil, ErrConflict
		}
		// 同内容重放：重建同一取件凭证明文返回。
		return l.deliveryReceipt(r, pickupToken(req.DeliveryToken, l.digestSecret)), nil

	default:
		return nil, stateActionError(r.state, true)
	}
}

// Pickup 使用取件凭证取件。成功取件后格口被释放且只释放一次；重复使用同一
// 凭证返回 [ErrAlreadyPickedUp]。格口已被释放并重新分配给后来的包裹后，
// 旧取件凭证不会影响新包裹。
func (l *Locker) Pickup(pickupTokenValue string) (*PickupReceipt, error) {
	if pickupTokenValue == "" {
		return nil, ErrCredentialInvalid
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.expireLocked(now)

	digest := l.digestSecret.tokenDigest(pickupTokenValue)
	id, ok := l.byPickup[digest]
	if !ok {
		if r := l.findByPickupDigestLocked(digest); r != nil {
			switch r.state {
			case StatePickedUp:
				return nil, ErrAlreadyPickedUp
			case StateExpired:
				return nil, ErrCredentialExpired
			}
		}
		// 无法匹配任何预约：可能是伪造凭证，也可能是属于已经被取消/
		// 释放并复用的旧预约 —— 一律按无效处理，绝不释放当前格口。
		return nil, ErrCredentialInvalid
	}
	r := l.reservations[id]

	if r.state != StateDelivered {
		// 索引理论上已被清理；防御性回落到状态错误。
		return nil, stateActionError(r.state, false)
	}
	if now.After(r.pickupExpiry) {
		l.expireReservationLocked(r, now)
		return nil, ErrCredentialExpired
	}

	parcel := *r.parcel
	ids := cloneStrings(r.compartmentIDs)
	r.state = StatePickedUp
	r.updatedAt = now

	// 精确一次释放：只有投递态取件会走到这里，delete 与占用标记清除
	// 都在同一临界区内，取消/过期并发到达时只有一方能完成释放。
	delete(l.byPickup, digest)
	delete(l.byDelivery, r.deliveryDigest)
	l.releaseCompartmentsLocked(r)

	l.recordLocked(HandoffRecord{
		Time:           now,
		Event:          EventPickedUp,
		ReservationID:  r.id,
		CompartmentIDs: ids,
		PickupHint:     shortHint(digest),
	})

	return &PickupReceipt{
		ReservationID:  r.id,
		CompartmentIDs: ids,
		Parcel:         parcel,
		PickedUpAt:     now,
	}, nil
}

// Cancel 在投递前取消预约并释放格口。对已投递、已取件、已取消或已过期的
// 预约取消将分别返回 [ErrConflict] / [ErrReservationCanceled] /
// [ErrCredentialExpired]。
func (l *Locker) Cancel(reservationID string) (*CancelReceipt, error) {
	if reservationID == "" {
		return nil, ErrInvalidRequest
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.expireLocked(now)

	r := l.reservations[reservationID]
	if r == nil {
		return nil, ErrNotFound
	}

	switch r.state {
	case StateReserved:
		ids := cloneStrings(r.compartmentIDs)
		r.state = StateCanceled
		r.updatedAt = now
		delete(l.byDelivery, r.deliveryDigest)
		l.releaseCompartmentsLocked(r)

		l.recordLocked(HandoffRecord{
			Time:           now,
			Event:          EventReservationCanceled,
			ReservationID:  r.id,
			CompartmentIDs: ids,
			DeliveryHint:   shortHint(r.deliveryDigest),
		})

		return &CancelReceipt{
			ReservationID:  r.id,
			CompartmentIDs: ids,
			CanceledAt:     now,
		}, nil
	case StateCanceled:
		return nil, ErrReservationCanceled
	case StateExpired:
		return nil, ErrCredentialExpired
	default: // delivered, pickedUp
		return nil, ErrConflict
	}
}

// SweepExpired 主动推进超时：把所有已过有效期的预约转为过期并释放其格口。
// 各业务方法在加锁后也会惰性执行同样的清理，因此不调用本方法也不会让过期
// 格口被错误投递/取件；本方法用于批量回收与状态查询前的推进。
func (l *Locker) SweepExpired() SweepResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.expireLocked(l.clock.Now())
}

// StatusView 是某一时刻快递柜的状态快照。
type StatusView struct {
	Time              time.Time
	TotalCompartments int
	Available         int
	Compartments      []CompartmentView
	Reservations      []ReservationStatus
}

// CompartmentView 是格口状态视图。
type CompartmentView struct {
	ID       string
	Size     Size
	Occupied bool
	// ReservationID 在占用时指向所属预约；空闲时为空。
	ReservationID string
}

// ReservationStatus 是预约状态视图，不含任何凭证明文。
type ReservationStatus struct {
	ReservationID  string
	State          ReservationState
	Size           Size
	CompartmentIDs []string
	Parcel         *ParcelInfo
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeliveryExpiry time.Time
	PickupExpiry   time.Time
}

// Status 先推进超时再返回当前状态快照。
func (l *Locker) Status() StatusView {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.expireLocked(now)

	view := StatusView{Time: now, TotalCompartments: len(l.compartments)}
	owner := make(map[string]string)
	compViews := make([]CompartmentView, 0, len(l.compartments))
	for _, c := range l.compartments {
		cv := CompartmentView{ID: c.id, Size: c.size, Occupied: c.occupied}
		compViews = append(compViews, cv)
		if !c.occupied {
			view.Available++
		}
	}
	sort.Slice(compViews, func(i, j int) bool { return compViews[i].ID < compViews[j].ID })
	for _, r := range l.reservations {
		if r.state == StateReserved || r.state == StateDelivered {
			for _, cid := range r.compartmentIDs {
				owner[cid] = r.id
			}
		}
	}
	for i := range compViews {
		compViews[i].ReservationID = owner[compViews[i].ID]
	}
	view.Compartments = compViews

	resViews := make([]ReservationStatus, 0, len(l.reservations))
	for _, r := range l.reservations {
		var parcelCopy *ParcelInfo
		if r.parcel != nil {
			p := *r.parcel
			parcelCopy = &p
		}
		resViews = append(resViews, ReservationStatus{
			ReservationID:  r.id,
			State:          r.state,
			Size:           r.size,
			CompartmentIDs: cloneStrings(r.compartmentIDs),
			Parcel:         parcelCopy,
			CreatedAt:      r.createdAt,
			UpdatedAt:      r.updatedAt,
			DeliveryExpiry: r.deliveryExpiry,
			PickupExpiry:   r.pickupExpiry,
		})
	}
	sort.Slice(resViews, func(i, j int) bool { return resViews[i].ReservationID < resViews[j].ReservationID })
	view.Reservations = resViews
	return view
}

// Reservation 返回单个预约的状态视图；不存在返回包装了 [ErrNotFound] 的错误。
func (l *Locker) Reservation(id string) (ReservationStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expireLocked(l.clock.Now())
	r := l.reservations[id]
	if r == nil {
		return ReservationStatus{}, ErrNotFound
	}
	var parcelCopy *ParcelInfo
	if r.parcel != nil {
		p := *r.parcel
		parcelCopy = &p
	}
	return ReservationStatus{
		ReservationID:  r.id,
		State:          r.state,
		Size:           r.size,
		CompartmentIDs: cloneStrings(r.compartmentIDs),
		Parcel:         parcelCopy,
		CreatedAt:      r.createdAt,
		UpdatedAt:      r.updatedAt,
		DeliveryExpiry: r.deliveryExpiry,
		PickupExpiry:   r.pickupExpiry,
	}, nil
}

// ---- 内部辅助（调用方必须持有 l.mu）----

// expireLocked 推进所有超时预约：投递窗口超时（reserved）或取件窗口超时
// （delivered）。返回本次释放的汇总。
func (l *Locker) expireLocked(now time.Time) SweepResult {
	var res SweepResult
	// 先收集 ID，避免在遍历 map 时修改状态。
	var expired []*reservation
	for _, r := range l.reservations {
		switch r.state {
		case StateReserved:
			if now.After(r.deliveryExpiry) {
				expired = append(expired, r)
			}
		case StateDelivered:
			if now.After(r.pickupExpiry) {
				expired = append(expired, r)
			}
		}
	}
	for _, r := range expired {
		res.ExpiredReservations = append(res.ExpiredReservations, r.id)
		res.ReleasedCompartments = append(res.ReleasedCompartments, r.compartmentIDs...)
		l.expireReservationLocked(r, now)
	}
	// 返回顺序不依赖 map 遍历：两个列表分别按字典序排列，便于调用方
	// 稳定地比较与记录批量回收结果。
	sort.Strings(res.ExpiredReservations)
	sort.Strings(res.ReleasedCompartments)
	return res
}

// expireReservationLocked 将单个预约转为过期并清理索引、释放格口。
// 对终态预约是 no-op，保证与取件/取消并发时格口只释放一次。
func (l *Locker) expireReservationLocked(r *reservation, now time.Time) {
	if r.state.Terminal() {
		return
	}
	event := EventDeliveryExpired
	if r.state == StateDelivered {
		event = EventPickupExpired
		delete(l.byPickup, r.pickupDigest)
	}
	delete(l.byDelivery, r.deliveryDigest)
	r.state = StateExpired
	r.updatedAt = now
	l.releaseCompartmentsLocked(r)

	l.recordLocked(HandoffRecord{
		Time:           now,
		Event:          event,
		ReservationID:  r.id,
		CompartmentIDs: cloneStrings(r.compartmentIDs),
		DeliveryHint:   shortHint(r.deliveryDigest),
		PickupHint:     shortHint(r.pickupDigest),
	})
}

// releaseCompartmentsLocked 只释放当前占用者仍为本预约的格口（fencing：
// occupantID 必须匹配）。二次调用，或在格口已被后来的新预约复用时调用，
// 都不会把新占用者误释放，因此“成功取件后格口只释放一次”不仅依赖状态机，
// 也由格口占用身份直接保证。
func (l *Locker) releaseCompartmentsLocked(r *reservation) {
	for _, cid := range r.compartmentIDs {
		c := l.compartments[cid]
		if c != nil && c.occupied && c.occupantID == r.id {
			c.occupied = false
			c.occupantID = ""
		}
	}
}

func (l *Locker) deliveryReceipt(r *reservation, pickup string) *DeliveryReceipt {
	return &DeliveryReceipt{
		ReservationID:   r.id,
		CompartmentIDs:  cloneStrings(r.compartmentIDs),
		PickupToken:     pickup,
		PickupExpiresAt: r.pickupExpiry,
		Parcel:          *r.parcel,
	}
}

func (l *Locker) findByDeliveryDigestLocked(digest string) *reservation {
	for _, r := range l.reservations {
		if equalDigest(r.deliveryDigest, digest) {
			return r
		}
	}
	return nil
}

func (l *Locker) findByPickupDigestLocked(digest string) *reservation {
	for _, r := range l.reservations {
		if r.pickupDigest != "" && equalDigest(r.pickupDigest, digest) {
			return r
		}
	}
	return nil
}

func (l *Locker) denyLocked(now time.Time, event HandoffEvent, r *reservation, deliveryDigest, pickupDigest, reason string) {
	l.recordLocked(HandoffRecord{
		Time:           now,
		Event:          event,
		ReservationID:  r.id,
		CompartmentIDs: cloneStrings(r.compartmentIDs),
		DeliveryHint:   shortHint(deliveryDigest),
		PickupHint:     shortHint(pickupDigest),
		Reason:         reason,
	})
}

func (l *Locker) recordLocked(rec HandoffRecord) {
	l.handoffs = append(l.handoffs, rec)
}

// stateActionError 把预约当前状态映射为对外错误。
func stateActionError(state ReservationState, deliveryAction bool) error {
	switch state {
	case StateCanceled:
		return ErrReservationCanceled
	case StateExpired:
		return ErrCredentialExpired
	case StatePickedUp:
		if deliveryAction {
			return ErrCredentialConsumed
		}
		return ErrAlreadyPickedUp
	case StateDelivered:
		if deliveryAction {
			// 正常的同内容重放不会走到这里。
			return ErrCredentialConsumed
		}
		return ErrConflict
	default:
		return ErrConflict
	}
}

func validateParcel(p ParcelInfo) error {
	if p.TrackingNumber == "" || !p.Size.Valid() || p.WeightGrams < 0 {
		return ErrInvalidRequest
	}
	return nil
}

func newReservationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实践中等同于整机不可用；保留 panic 路径。
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
