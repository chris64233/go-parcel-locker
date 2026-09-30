package goparcellocker

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// fencing 不变量：格口始终知道当前占用者的预约 ID；旧预约的释放路径
// （取件 / 取消 / 超时）在格口已被新预约复用后不会动到新占用者。
func TestCompartmentOccupantFencing(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	r1, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	l.mu.RLock()
	c := l.compartments["X"]
	if !c.occupied || c.occupantID != r1.ReservationID {
		t.Fatalf("occupant after reserve = %q occupied=%v, want %s", c.occupantID, c.occupied, r1.ReservationID)
	}
	l.mu.RUnlock()

	if _, err := l.Cancel(r1.ReservationID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	l.mu.RLock()
	if c.occupied || c.occupantID != "" {
		t.Fatalf("occupant after cancel = %q occupied=%v, want empty/free", c.occupantID, c.occupied)
	}
	l.mu.RUnlock()

	// 第二代占用者。
	r2, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	d2, err := l.Deliver(DeliveryRequest{DeliveryToken: r2.DeliveryToken, Parcel: sampleParcel(SizeMedium, "GEN2")})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	l.mu.RLock()
	if !c.occupied || c.occupantID != r2.ReservationID {
		t.Fatalf("occupant after reuse = %q, want %s", c.occupantID, r2.ReservationID)
	}
	// 直接以旧预约身份走内部释放路径：fencing 必须拒绝释放新占用者。
	old := l.reservations[r1.ReservationID]
	l.mu.RUnlock()
	l.mu.Lock()
	l.releaseCompartmentsLocked(old)
	freed := !c.occupied
	l.mu.Unlock()
	if freed {
		t.Fatal("stale release must not free a compartment owned by a newer reservation")
	}
	st := l.Status()
	if st.Available != 0 || st.Compartments[0].ReservationID != r2.ReservationID {
		t.Fatalf("new occupant must remain in place, available=%d", st.Available)
	}

	if _, err := l.Pickup(d2.PickupToken); err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	l.mu.RLock()
	if c.occupied || c.occupantID != "" {
		t.Fatalf("occupant after pickup = %q occupied=%v, want empty/free", c.occupantID, c.occupied)
	}
	l.mu.RUnlock()
}

// 多格口整笔预约 + 批量超时：SweepResult 的两个列表必须确定有序，
// 且被释放的格口立即可被新预约复用。
func TestSweepExpiredDeterministicOrder(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	ids := []string{"A", "B", "C", "D", "E"}
	configs := make([]CompartmentConfig, len(ids))
	for i, id := range ids {
		configs[i] = CompartmentConfig{ID: id, Size: SizeSmall}
	}
	if err := l.AddCompartments(configs...); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	// 一笔 2 格口预约 + 两笔单格口预约，随后全部跨过投递窗口。
	big, err := l.Reserve(ReservationRequest{Size: SizeSmall, Count: 2})
	if err != nil {
		t.Fatalf("Reserve big: %v", err)
	}
	r1, _ := l.Reserve(ReservationRequest{Size: SizeSmall})
	r2, _ := l.Reserve(ReservationRequest{Size: SizeSmall})
	clock.advance(11 * time.Minute)

	res := l.SweepExpired()
	wantRes := []string{r1.ReservationID, r2.ReservationID, big.ReservationID}
	sort.Strings(wantRes)
	if len(res.ExpiredReservations) != len(wantRes) {
		t.Fatalf("expired reservations = %v, want %v", res.ExpiredReservations, wantRes)
	}
	for i := range wantRes {
		if res.ExpiredReservations[i] != wantRes[i] {
			t.Fatalf("expired reservations not sorted: %v", res.ExpiredReservations)
		}
	}
	// E 从未被预约占用，不在“本次释放”列表中；被释放的是 A-D。
	wantReleased := []string{"A", "B", "C", "D"}
	if len(res.ReleasedCompartments) != len(wantReleased) {
		t.Fatalf("released compartments = %v, want %v", res.ReleasedCompartments, wantReleased)
	}
	for i := range wantReleased {
		if res.ReleasedCompartments[i] != wantReleased[i] {
			t.Fatalf("released compartments not sorted: %v", res.ReleasedCompartments)
		}
	}
	if got := l.Status().Available; got != len(ids) {
		t.Fatalf("all compartments free after sweep, available=%d", got)
	}
	// 释放后的多个格口可整笔重新预约。
	re, err := l.Reserve(ReservationRequest{Size: SizeSmall, Count: 5})
	if err != nil {
		t.Fatalf("reserve after sweep: %v", err)
	}
	if len(re.CompartmentIDs) != 5 {
		t.Fatalf("expected 5 compartments, got %v", re.CompartmentIDs)
	}
}

// 终态预约上的迟到操作返回明确且稳定的错误分类。
func TestStaleOperationsOnTerminalReservations(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock, WithDeliveryTTL(10*time.Minute), WithPickupTTL(time.Hour))
	_ = l.AddCompartments(
		CompartmentConfig{ID: "P", Size: SizeMedium},
		CompartmentConfig{ID: "C", Size: SizeMedium},
		CompartmentConfig{ID: "E", Size: SizeMedium},
		CompartmentConfig{ID: "X", Size: SizeMedium},
	)
	p := func(tag string) ParcelInfo { return sampleParcel(SizeMedium, tag) }

	// picked_up：迟到投递 -> consumed；重复取件 -> already picked up。
	rr, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	dd, _ := l.Deliver(DeliveryRequest{DeliveryToken: rr.DeliveryToken, Parcel: p("P")})
	if _, err := l.Pickup(dd.PickupToken); err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: rr.DeliveryToken, Parcel: p("P")}); !errors.Is(err, ErrCredentialConsumed) {
		t.Fatalf("late deliver after pickup: %v", err)
	}
	if _, err := l.Pickup(dd.PickupToken); !errors.Is(err, ErrAlreadyPickedUp) {
		t.Fatalf("double pickup: %v", err)
	}

	// canceled：重复取消 -> canceled；迟到投递 -> canceled。
	rr, _ = l.Reserve(ReservationRequest{Size: SizeMedium})
	if _, err := l.Cancel(rr.ReservationID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := l.Cancel(rr.ReservationID); !errors.Is(err, ErrReservationCanceled) {
		t.Fatalf("double cancel: %v", err)
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: rr.DeliveryToken, Parcel: p("C")}); !errors.Is(err, ErrReservationCanceled) {
		t.Fatalf("deliver after cancel: %v", err)
	}

	// delivery expiry：迟到投递 -> expired；取消 -> expired。
	rr, _ = l.Reserve(ReservationRequest{Size: SizeMedium})
	clock.advance(11 * time.Minute)
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: rr.DeliveryToken, Parcel: p("E")}); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("deliver after delivery expiry: %v", err)
	}
	if _, err := l.Cancel(rr.ReservationID); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("cancel after delivery expiry: %v", err)
	}

	// pickup expiry：迟到取件 -> expired。
	rr, _ = l.Reserve(ReservationRequest{Size: SizeMedium})
	dd, _ = l.Deliver(DeliveryRequest{DeliveryToken: rr.DeliveryToken, Parcel: p("X")})
	clock.advance(2 * time.Hour)
	if _, err := l.Pickup(dd.PickupToken); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("pickup after pickup expiry: %v", err)
	}
	if l.Status().Available != 4 {
		t.Fatalf("all compartments should be free, available=%d", l.Status().Available)
	}
}

// 并发混合终止（取件 / 取消 / 迟到投递）下，每个格口只释放一次，
// 终态唯一且不残留占用身份与活跃凭证索引。
func TestConcurrentTerminalTransitionsFencing(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	const n = 12
	configs := make([]CompartmentConfig, n)
	for i := 0; i < n; i++ {
		configs[i] = CompartmentConfig{ID: fmt.Sprintf("C%02d", i), Size: SizeSmall}
	}
	if err := l.AddCompartments(configs...); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	type item struct {
		r *ReservationReceipt
		d *DeliveryReceipt
	}
	items := make([]item, n)
	for i := range items {
		r, err := l.Reserve(ReservationRequest{Size: SizeSmall})
		if err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
		// 一半停留在 reserved（参与取消/迟到投递竞态），一半完成投递
		// （参与取件/取消竞态）。
		if i%2 == 0 {
			items[i] = item{r: r}
		} else {
			d, err := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: sampleParcel(SizeSmall, fmt.Sprintf("T%d", i))})
			if err != nil {
				t.Fatalf("Deliver %d: %v", i, err)
			}
			items[i] = item{r: r, d: d}
		}
	}

	var (
		wg        sync.WaitGroup
		delMu     sync.Mutex
		successes []*DeliveryReceipt
	)
	start := make(chan struct{})
	for i := range items {
		it := items[i]
		idx := i
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, _ = l.Cancel(it.r.ReservationID) }()
		go func() {
			defer wg.Done()
			<-start
			var rec *DeliveryReceipt
			if it.d != nil {
				rec = it.d
				_, _ = l.Pickup(it.d.PickupToken)
			} else if d, err := l.Deliver(DeliveryRequest{DeliveryToken: it.r.DeliveryToken, Parcel: sampleParcel(SizeSmall, fmt.Sprintf("L%d", idx))}); err == nil {
				// 竞态中迟到投递若获胜，包裹合法入柜：登记回执稍后取走。
				rec = d
			}
			if rec != nil {
				delMu.Lock()
				successes = append(successes, rec)
				delMu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	// 竞态中成功投递入柜的包裹全部完成取件（重复取件视为已终结）。
	for _, d := range successes {
		if _, err := l.Pickup(d.PickupToken); err != nil && !errors.Is(err, ErrAlreadyPickedUp) {
			t.Fatalf("draining pickup: %v", err)
		}
	}

	st := l.Status()
	if st.Available != n {
		t.Fatalf("every compartment must be free exactly once, available=%d", st.Available)
	}
	for _, rv := range st.Reservations {
		if !rv.State.Terminal() {
			t.Fatalf("reservation %s not terminal: %s", rv.ReservationID, rv.State)
		}
	}
	l.mu.RLock()
	for _, c := range l.compartments {
		if c.occupied || c.occupantID != "" {
			t.Fatalf("compartment %s left occupied=%v occupant=%q", c.id, c.occupied, c.occupantID)
		}
	}
	indexes := len(l.byDelivery) + len(l.byPickup)
	l.mu.RUnlock()
	if indexes != 0 {
		t.Fatal("active credential indexes must be empty after all terminals")
	}
}
