// Package goparcellocker 实现快递柜格口的预约、投递与一次性取件流程。
//
// 核心保证：
//
//   - 整笔预约是原子的：多包裹要么全部拿到“仍可用且尺寸合适”的格口，
//     要么一个都不占用；并发预约绝不会把同一格口分给两个包裹。
//   - 投递凭证与取件码均为一次性凭证，系统内只保存 HMAC 摘要，不存明文。
//   - 同一投递凭证携带相同包裹内容重复提交是幂等的，返回首次结果（含相同
//     取件码）；包裹信息变化则明确报冲突。
//   - 取件、取消、超时推进并发到达时，格口至多释放一次；旧凭证/旧请求不会
//     影响后来使用同一格口的新包裹（每个条目带 generation 代际标识）。
//   - 交接记录只含凭证摘要与短标签，任何接口都不返回凭证明文。
//
// Locker 的所有方法都可被多个 goroutine 并发调用。
package goparcellocker

import (
	"crypto/rand"
	"sort"
	"sync"
	"time"
)

const (
	// defaultDeliveryTTL 预约的默认投递有效期。
	defaultDeliveryTTL = 15 * time.Minute
	// defaultPickupTTL 投递后的默认取件有效期。
	defaultPickupTTL = 48 * time.Hour
	// tokenEntropyBytes 投递凭证的随机熵字节数（base32 编码后约 26 个字符）。
	tokenEntropyBytes = 16
	hmacKeyBytes      = 32
)

// Option 用于配置 NewLocker。
type Option func(*Locker)

// WithClock 注入时间源（测试用）。默认使用 time.Now。
func WithClock(clock func() time.Time) Option {
	return func(l *Locker) {
		if clock != nil {
			l.clock = clock
		}
	}
}

// WithDeliveryTTL 设置预约投递有效期，d 必须为正，否则忽略。
func WithDeliveryTTL(d time.Duration) Option {
	return func(l *Locker) {
		if d > 0 {
			l.deliveryTTL = d
		}
	}
}

// WithPickupTTL 设置投递后的取件有效期，d 必须为正，否则忽略。
func WithPickupTTL(d time.Duration) Option {
	return func(l *Locker) {
		if d > 0 {
			l.pickupTTL = d
		}
	}
}

// WithHMACKey 注入凭证摘要/派生使用的服务端密钥（主要供需要确定性的测试使用）。
// key 至少应有 32 字节；为空时忽略并使用随机密钥。
func WithHMACKey(key []byte) Option {
	return func(l *Locker) {
		if len(key) > 0 {
			l.hmacKey = append([]byte(nil), key...)
		}
	}
}

// Locker 是快递柜系统的内存实现。零值不可用，请使用 NewLocker 创建。
type Locker struct {
	mu sync.Mutex

	hmacKey     []byte
	clock       func() time.Time
	deliveryTTL time.Duration
	pickupTTL   time.Duration
	generation  int64

	// compartments 格口静态配置（重配时整体替换）。
	compartments map[string]*compartment
	// sortedCompartments 按 (尺寸, ID) 升序，供“最佳适配”线性扫描。
	sortedCompartments []*compartment

	reservations  map[string]*reservation
	deliveryCreds map[string]*deliveryCredential
	pickupCreds   map[string]*pickupCredential
	records       []HandoffRecord
}

type compartment struct {
	id   string
	size Size
	// occupant 指向当前占用代际；nil 表示格口空闲可用。
	occupant *occupancy
}

// occupancy 是格口占用者的代际标识。
type occupancy struct {
	reservationID string
	itemID        int
	gen           int64
}

type item struct {
	id         int
	trackingNo string
	size       Size
	// contentFP 预约时登记的包裹信息摘要（尺寸 + 运单号 + 内容摘要）。
	contentFP string
	status    ItemStatus

	compartmentID string
	gen           int64

	deliveredFP    string
	pickupFP       string
	deliveredAt    time.Time
	pickupDeadline time.Time
	expiredPhase   string
}

type reservation struct {
	id               string
	courierID        string
	items            []*item
	reservedAt       time.Time
	deliveryDeadline time.Time
}

type deliveryCredential struct {
	reservationID string
	itemID        int
	gen           int64
}

type pickupCredential struct {
	reservationID string
	itemID        int
	gen           int64
}

// NewLocker 创建一个尚未配置格口的快递柜，可用 Option 调整时间源与有效期。
// 服务端 HMAC 密钥默认随机生成；仅当系统随机源不可用时才会 panic。
func NewLocker(opts ...Option) *Locker {
	key := make([]byte, hmacKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic("goparcellocker: cannot initialize hmac key: " + err.Error())
	}
	l := &Locker{
		hmacKey:       key,
		clock:         time.Now,
		deliveryTTL:   defaultDeliveryTTL,
		pickupTTL:     defaultPickupTTL,
		compartments:  map[string]*compartment{},
		reservations:  map[string]*reservation{},
		deliveryCreds: map[string]*deliveryCredential{},
		pickupCreds:   map[string]*pickupCredential{},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// ConfigureCompartments 整体设置格口配置。
// 若仍存在未终结（待投递/待取件）的条目，则拒绝重配并返回 ErrCompartmentsInUse；
// 历史交接记录始终保留。
func (l *Locker) ConfigureCompartments(specs []CompartmentSpec) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(specs) == 0 {
		return errWrapf(ErrInvalidRequest, "at least one compartment is required")
	}
	next := make(map[string]*compartment, len(specs))
	for _, spec := range specs {
		if spec.ID == "" {
			return errWrapf(ErrInvalidRequest, "compartment id must not be empty")
		}
		if !spec.Size.Valid() {
			return errWrapf(ErrInvalidRequest, "compartment %q has invalid size", spec.ID)
		}
		if _, dup := next[spec.ID]; dup {
			return errWrapf(ErrInvalidRequest, "duplicate compartment id %q", spec.ID)
		}
		next[spec.ID] = &compartment{id: spec.ID, size: spec.Size}
	}

	for _, r := range l.reservations {
		for _, it := range r.items {
			if it.status == ItemReserved || it.status == ItemDelivered {
				return errWrapf(ErrCompartmentsInUse, "reservation %s item %d still active", r.id, it.id)
			}
		}
	}

	l.compartments = next
	l.sortedCompartments = l.sortedCompartments[:0]
	for _, c := range next {
		l.sortedCompartments = append(l.sortedCompartments, c)
	}
	sort.Slice(l.sortedCompartments, func(i, j int) bool {
		a, b := l.sortedCompartments[i], l.sortedCompartments[j]
		if a.size != b.size {
			return a.size < b.size
		}
		return a.id < b.id
	})
	return nil
}

// Reserve 为快递员一次性预约整批包裹。
//
// 分配策略为“最佳适配”（能容纳的最小尺寸，尺寸相同取 ID 最小者），
// 并按包裹尺寸从大到小依次落位以保证可行性。任一包裹找不到合适格口时，
// 整笔预约失败且不会提前占用任何格口。
//
// 返回的投递凭证明文只出现这一次；deliveryDeadline 为零值时使用默认有效期。
func (l *Locker) Reserve(courierID string, packages []Package, deliveryDeadline time.Time) (*ReserveReceipt, error) {
	if len(packages) == 0 {
		return nil, errWrapf(ErrInvalidRequest, "at least one package is required")
	}
	for i, p := range packages {
		if !p.Size.Valid() {
			return nil, errWrapf(ErrInvalidRequest, "package %d has invalid size", i)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	l.sweepLocked(now)

	if deliveryDeadline.IsZero() {
		deliveryDeadline = now.Add(l.deliveryTTL)
	}
	if !deliveryDeadline.After(now) {
		return nil, ErrInvalidDeadline
	}

	// 按尺寸降序排列请求下标（同尺寸保持原顺序），先给大包裹找位。
	order := make([]int, len(packages))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return packages[order[a]].Size > packages[order[b]].Size
	})

	// 先在不修改状态的前提下产出完整分配方案。
	plan := make([]*compartment, len(packages))
	for _, idx := range order {
		pkg := packages[idx]
		var chosen *compartment
		// sortedCompartments 按 (尺寸, ID) 升序，第一个“空闲且够大”的即最佳适配。
		for _, c := range l.sortedCompartments {
			if c.occupant == nil && c.size >= pkg.Size {
				chosen = c
				break
			}
		}
		if chosen == nil {
			// 整笔失败：撤销本批次已设置的临时占位，不占用任何格口。
			l.rollbackPlan(plan)
			return nil, errWrapf(ErrNoAvailableCompartment, "package %d (tracking %q, size %s)",
				idx, pkg.TrackingNo, pkg.Size)
		}
		chosen.occupant = &occupancy{} // 占位，防止同批次后续包裹重复选中
		plan[idx] = chosen
	}

	// 方案完整。先生成全部随机产物，任何一步失败都能在提交前中止。
	reservationID, err := l.uniqueReservationID()
	if err != nil {
		l.rollbackPlan(plan)
		return nil, err
	}
	tokens := make([]string, len(packages))
	tokenFPs := make([]string, len(packages))
	for i := range packages {
		tok, err := randomToken(tokenEntropyBytes)
		if err != nil {
			l.rollbackPlan(plan)
			return nil, err
		}
		tokens[i] = tok
		tokenFPs[i] = credentialFingerprint(l.hmacKey, tok)
		if _, exists := l.deliveryCreds[tokenFPs[i]]; exists {
			l.rollbackPlan(plan)
			return nil, errWrapf(ErrInvalidRequest, "credential collision, please retry")
		}
	}

	r := &reservation{
		id:               reservationID,
		courierID:        courierID,
		reservedAt:       now,
		deliveryDeadline: deliveryDeadline,
	}
	items := make([]ReservedItem, len(packages))
	for i, pkg := range packages {
		l.generation++
		c := plan[i]
		it := &item{
			id:            i + 1,
			trackingNo:    pkg.TrackingNo,
			size:          pkg.Size,
			contentFP:     contentFingerprint(pkg),
			status:        ItemReserved,
			compartmentID: c.id,
			gen:           l.generation,
		}
		c.occupant = &occupancy{reservationID: reservationID, itemID: it.id, gen: it.gen}
		r.items = append(r.items, it)

		l.deliveryCreds[tokenFPs[i]] = &deliveryCredential{
			reservationID: reservationID,
			itemID:        it.id,
			gen:           it.gen,
		}
		l.appendRecordLocked(now, EventReserved, r, it, tokenFPs[i], "")
		items[i] = ReservedItem{
			ItemID:          it.id,
			CompartmentID:   c.id,
			Size:            pkg.Size,
			TrackingNo:      pkg.TrackingNo,
			DeliveryToken:   tokens[i],
			DeliveryExpires: deliveryDeadline,
		}
	}
	l.reservations[reservationID] = r

	return &ReserveReceipt{
		ReservationID:    reservationID,
		DeliveryDeadline: deliveryDeadline,
		Items:            items,
	}, nil
}

// rollbackPlan 撤销分配阶段在格口上设置的临时占位。
func (l *Locker) rollbackPlan(plan []*compartment) {
	for _, c := range plan {
		if c != nil && c.occupant != nil && c.occupant.reservationID == "" {
			c.occupant = nil
		}
	}
}

func (l *Locker) uniqueReservationID() (string, error) {
	for range 5 {
		id, err := randomID()
		if err != nil {
			return "", err
		}
		if _, exists := l.reservations[id]; !exists {
			return id, nil
		}
	}
	return "", errWrapf(ErrInvalidRequest, "cannot allocate reservation id, please retry")
}

// Deliver 使用一次性投递凭证完成投递。
//
//   - 首次投递：包裹信息必须与预约时一致，成功后生成取件凭证，投递凭证即失效；
//   - 相同凭证 + 相同包裹内容的重复请求：幂等返回首次结果（含同一个取件码）；
//   - 相同凭证 + 不同包裹信息：返回包装了 ErrCredentialReplayed 或
//     ErrPackageConflict 的错误，明确报冲突；
//   - 预约已取消/已过期：投递被拒绝。
func (l *Locker) Deliver(deliveryToken string, pkg Package) (*DeliveryReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	l.sweepLocked(now)

	fp := credentialFingerprint(l.hmacKey, deliveryToken)
	cred, ok := l.deliveryCreds[fp]
	if !ok {
		return nil, ErrCredentialInvalid
	}
	r, it, err := l.locateItem(cred.reservationID, cred.itemID, cred.gen)
	if err != nil {
		return nil, err
	}
	incoming := contentFingerprint(pkg)

	switch it.status {
	case ItemReserved:
		if !constantTimeEqual(incoming, it.contentFP) {
			return nil, errWrapf(ErrPackageConflict,
				"delivered package conflicts with reservation %s item %d", r.id, it.id)
		}
		it.status = ItemDelivered
		it.deliveredFP = incoming
		it.deliveredAt = now
		it.pickupDeadline = now.Add(l.pickupTTL)

		pickupCode := derivePickupCode(l.hmacKey, deliveryToken)
		pickupFP := credentialFingerprint(l.hmacKey, pickupCode)
		it.pickupFP = pickupFP
		l.pickupCreds[pickupFP] = &pickupCredential{
			reservationID: r.id,
			itemID:        it.id,
			gen:           it.gen,
		}
		l.appendRecordLocked(now, EventDelivered, r, it, fp, "")
		return l.deliveryReceipt(r, it, pickupCode, false), nil

	case ItemDelivered, ItemPickedUp:
		// 凭证已消费过：内容一致则幂等重放，不一致则明确冲突。
		if !constantTimeEqual(incoming, it.deliveredFP) {
			return nil, errWrapf(ErrCredentialReplayed,
				"reservation %s item %d", r.id, it.id)
		}
		// 取件码由投递凭证确定性派生，不入库明文也能稳定重现。
		pickupCode := derivePickupCode(l.hmacKey, deliveryToken)
		return l.deliveryReceipt(r, it, pickupCode, true), nil

	case ItemCanceled:
		return nil, errWrapf(ErrReservationCanceled, "reservation %s item %d", r.id, it.id)
	case ItemExpired:
		return nil, l.expiredItemError(it)
	default:
		return nil, ErrCredentialInvalid
	}
}

// ErrPackageConflict 在 errors.go 中与其他错误一起声明。
var _ = ErrPackageConflict

func (l *Locker) deliveryReceipt(r *reservation, it *item, pickupCode string, replayed bool) *DeliveryReceipt {
	return &DeliveryReceipt{
		ReservationID:  r.id,
		ItemID:         it.id,
		CompartmentID:  it.compartmentID,
		TrackingNo:     it.trackingNo,
		PickupCode:     pickupCode,
		DeliveredAt:    it.deliveredAt,
		PickupDeadline: it.pickupDeadline,
		Replayed:       replayed,
	}
}

func (l *Locker) expiredItemError(it *item) error {
	if it.expiredPhase == "pickup" {
		return errWrapf(ErrPackageExpired, "item %d", it.id)
	}
	return errWrapf(ErrReservationExpired, "item %d", it.id)
}

// Pickup 使用一次性取件码取件。成功后格口立即释放且只释放一次，
// 取件码随即失效；过期、取消或属于旧一代格口使用者的取件请求都会被拒绝。
func (l *Locker) Pickup(pickupCode string) (*PickupReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	l.sweepLocked(now)

	fp := credentialFingerprint(l.hmacKey, pickupCode)
	cred, ok := l.pickupCreds[fp]
	if !ok {
		return nil, ErrCredentialInvalid
	}
	r, it, err := l.locateItem(cred.reservationID, cred.itemID, cred.gen)
	if err != nil {
		return nil, err
	}

	switch it.status {
	case ItemDelivered:
		// 代际 + 占用者双重校验：旧取件码不可能动到新占用者的格口。
		c := l.compartments[it.compartmentID]
		if c == nil || c.occupant == nil || c.occupant.gen != it.gen {
			return nil, ErrCredentialInvalid
		}
		c.occupant = nil // 格口只释放一次：取件是唯一的释放点之一，状态机保证不重复
		it.status = ItemPickedUp
		delete(l.pickupCreds, fp)
		l.appendRecordLocked(now, EventPickedUp, r, it, fp, "")
		return &PickupReceipt{
			ReservationID: r.id,
			ItemID:        it.id,
			CompartmentID: it.compartmentID,
			TrackingNo:    it.trackingNo,
			PickedUpAt:    now,
		}, nil
	case ItemExpired:
		return nil, l.expiredItemError(it)
	case ItemPickedUp:
		return nil, ErrCredentialInvalid
	default:
		return nil, errWrapf(ErrPackageNotReady, "reservation %s item %d", r.id, it.id)
	}
}

// CancelReservation 取消整笔预约并释放其全部格口。
//
// 仅当所有包裹都仍处于待投递状态时才能整笔取消；只要有任意包裹已投递或已
// 终结，取消即被拒绝（避免拆散已在途的交接），返回 ErrReservationNotActive。
func (l *Locker) CancelReservation(reservationID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	l.sweepLocked(now)

	r, ok := l.reservations[reservationID]
	if !ok {
		return errWrapf(ErrReservationNotFound, "reservation %s", reservationID)
	}
	for _, it := range r.items {
		if it.status != ItemReserved {
			return errWrapf(ErrReservationNotActive, "reservation %s item %d is %s",
				reservationID, it.id, it.status)
		}
	}
	for _, it := range r.items {
		it.status = ItemCanceled
		l.releaseCompartmentLocked(it)
		// 投递凭证不在此处删除：保留为“已撤销”凭据（墓碑），
		// 之后再投递时能明确返回 ErrReservationCanceled 而非笼统的无效凭证。
		l.appendRecordLocked(now, EventCanceled, r, it, "", "")
	}
	return nil
}

// Advance 将逻辑时钟推进到 now（零值表示使用当前时间），终结所有已过截止时间的
// 待投递/待取件条目并释放对应格口，返回本次被过期处理的条目。
// 所有写操作内部也会自动推进，因此显式调用主要用于测试与定时任务。
func (l *Locker) Advance(now time.Time) []ExpiredItem {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.IsZero() {
		now = l.clock()
	}
	return l.sweepLocked(now)
}

// sweepLocked 执行超时释放。调用方必须持有 l.mu。
func (l *Locker) sweepLocked(now time.Time) []ExpiredItem {
	var expired []ExpiredItem

	ids := make([]string, 0, len(l.reservations))
	for id := range l.reservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		r := l.reservations[id]
		for _, it := range r.items {
			switch {
			case it.status == ItemReserved && !now.Before(r.deliveryDeadline):
				it.status = ItemExpired
				it.expiredPhase = "delivery"
				l.releaseCompartmentLocked(it)
				// 投递凭证保留为墓碑，旧凭证再来时返回 ErrReservationExpired。
				l.appendRecordLocked(now, EventExpired, r, it, "", "delivery")
				expired = append(expired, ExpiredItem{
					ReservationID: r.id,
					ItemID:        it.id,
					CompartmentID: it.compartmentID,
					Phase:         "delivery",
				})
			case it.status == ItemDelivered && !now.Before(it.pickupDeadline):
				it.status = ItemExpired
				it.expiredPhase = "pickup"
				l.releaseCompartmentLocked(it)
				// 取件凭证保留为墓碑：旧取件码再来时返回 ErrPackageExpired，
				// 且其代际指向旧条目，不可能影响后来使用该格口的新包裹。
				l.appendRecordLocked(now, EventExpired, r, it, "", "pickup")
				expired = append(expired, ExpiredItem{
					ReservationID: r.id,
					ItemID:        it.id,
					CompartmentID: it.compartmentID,
					Phase:         "pickup",
				})
			}
		}
	}
	return expired
}

// releaseCompartmentLocked 仅当格口当前占用者确为该条目代际时才释放，
// 从根本上杜绝旧条目误释放新包裹占用的格口。调用方持有 l.mu。
func (l *Locker) releaseCompartmentLocked(it *item) {
	c, ok := l.compartments[it.compartmentID]
	if !ok || c.occupant == nil {
		return
	}
	if c.occupant.reservationID == "" || c.occupant.gen != it.gen {
		return
	}
	c.occupant = nil
}

func (l *Locker) locateItem(reservationID string, itemID int, gen int64) (*reservation, *item, error) {
	r, ok := l.reservations[reservationID]
	if !ok {
		return nil, nil, ErrCredentialInvalid
	}
	for _, it := range r.items {
		if it.id == itemID {
			if it.gen != gen {
				// 凭证属于该格口的旧一代使用者。
				return nil, nil, ErrCredentialInvalid
			}
			return r, it, nil
		}
	}
	return nil, nil, ErrCredentialInvalid
}

func (l *Locker) appendRecordLocked(at time.Time, kind EventKind, r *reservation, it *item, credentialFP, detail string) {
	rec := HandoffRecord{
		At:            at,
		Kind:          kind,
		ReservationID: r.id,
		ItemID:        it.id,
		CompartmentID: it.compartmentID,
		TrackingNo:    it.trackingNo,
		CourierID:     r.courierID,
		Detail:        detail,
	}
	if credentialFP != "" {
		rec.CredentialFingerprint = credentialFP
		rec.CredentialHint = fingerprintHint(credentialFP)
	}
	l.records = append(l.records, rec)
}

// Compartments 返回全部格口的当前状态副本，按 ID 排序。
func (l *Locker) Compartments() []CompartmentState {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]CompartmentState, 0, len(l.compartments))
	for _, c := range l.sortedCompartments {
		st := CompartmentState{ID: c.id, Size: c.size}
		if c.occupant != nil {
			st.InUse = true
			st.ReservationID = c.occupant.reservationID
			st.ItemID = c.occupant.itemID
		}
		out = append(out, st)
	}
	return out
}

// Reservation 返回单笔预约的状态快照；不存在时包装 ErrReservationNotFound。
func (l *Locker) Reservation(reservationID string) (ReservationStatus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	r, ok := l.reservations[reservationID]
	if !ok {
		return ReservationStatus{}, errWrapf(ErrReservationNotFound, "reservation %s", reservationID)
	}
	return l.reservationViewLocked(r), nil
}

func (l *Locker) reservationViewLocked(r *reservation) ReservationStatus {
	view := ReservationStatus{
		ID:               r.id,
		CourierID:        r.courierID,
		ReservedAt:       r.reservedAt,
		DeliveryDeadline: r.deliveryDeadline,
		Items:            make([]ItemView, 0, len(r.items)),
	}
	for _, it := range r.items {
		view.Items = append(view.Items, ItemView{
			ItemID:         it.id,
			TrackingNo:     it.trackingNo,
			Size:           it.size,
			Status:         it.status,
			CompartmentID:  it.compartmentID,
			DeliveredAt:    it.deliveredAt,
			PickupDeadline: it.pickupDeadline,
		})
		if it.status == ItemReserved {
			view.Active = true
		}
	}
	return view
}

// Status 返回系统整体状态快照（格口、预约、按尺寸统计的空闲/占用数）。
func (l *Locker) Status() StatusSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()

	snap := StatusSnapshot{
		Compartments: make([]CompartmentState, 0, len(l.sortedCompartments)),
		FreeBySize:   map[Size]int{SizeSmall: 0, SizeMedium: 0, SizeLarge: 0},
		UsedBySize:   map[Size]int{SizeSmall: 0, SizeMedium: 0, SizeLarge: 0},
	}
	for _, c := range l.sortedCompartments {
		st := CompartmentState{ID: c.id, Size: c.size}
		if c.occupant != nil {
			st.InUse = true
			st.ReservationID = c.occupant.reservationID
			st.ItemID = c.occupant.itemID
			snap.UsedBySize[c.size]++
		} else {
			snap.FreeBySize[c.size]++
		}
		snap.Compartments = append(snap.Compartments, st)
	}

	ids := make([]string, 0, len(l.reservations))
	for id := range l.reservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snap.Reservations = make([]ReservationStatus, 0, len(ids))
	for _, id := range ids {
		snap.Reservations = append(snap.Reservations, l.reservationViewLocked(l.reservations[id]))
	}
	return snap
}

// HandoffRecords 返回交接记录副本。记录只含凭证摘要与短标签，不含任何凭证明文。
func (l *Locker) HandoffRecords() []HandoffRecord {
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]HandoffRecord, len(l.records))
	copy(out, l.records)
	return out
}
