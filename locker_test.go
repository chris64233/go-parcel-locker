package goparcellocker

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLocker(t *testing.T, clock *fakeClock, opts ...Option) *Locker {
	t.Helper()
	if clock == nil {
		clock = newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	}
	allOpts := append([]Option{WithClock(clock)}, opts...)
	l, err := NewLocker(allOpts...)
	if err != nil {
		t.Fatalf("NewLocker: %v", err)
	}
	return l
}

func sampleParcel(size Size, tracking string) ParcelInfo {
	return ParcelInfo{
		TrackingNumber: tracking,
		Size:           size,
		WeightGrams:    1200,
		Sender:         "sender-A",
		Recipient:      "recipient-B",
	}
}

// 全流程：预约 -> 投递 -> 取件，格口取件后恢复可用。
func TestHappyPath(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(
		CompartmentConfig{ID: "S1", Size: SizeSmall},
		CompartmentConfig{ID: "M1", Size: SizeMedium},
	); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	res, err := l.Reserve(ReservationRequest{Size: SizeSmall})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// best-fit：小包裹应分到小格口，而不是占走中格口。
	if len(res.CompartmentIDs) != 1 || res.CompartmentIDs[0] != "S1" {
		t.Fatalf("expected S1, got %v", res.CompartmentIDs)
	}
	if !strings.HasPrefix(res.DeliveryToken, deliveryPrefix) {
		t.Fatalf("delivery token prefix mismatch: %q", res.DeliveryToken)
	}

	st := l.Status()
	if st.Available != 1 {
		t.Fatalf("expected 1 available after reserve, got %d", st.Available)
	}

	parcel := sampleParcel(SizeSmall, "TRK-1")
	del, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: parcel})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.HasPrefix(del.PickupToken, pickupPrefix) {
		t.Fatalf("pickup token prefix mismatch: %q", del.PickupToken)
	}
	if del.PickupToken == res.DeliveryToken {
		t.Fatal("pickup token must differ from delivery token")
	}

	pick, err := l.Pickup(del.PickupToken)
	if err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	if pick.Parcel.TrackingNumber != "TRK-1" {
		t.Fatalf("unexpected parcel: %+v", pick.Parcel)
	}

	st = l.Status()
	if st.Available != 2 {
		t.Fatalf("expected all compartments free after pickup, available=%d", st.Available)
	}
}

// 多格口预约整笔成功 / 容量不足整笔失败且不占格。
func TestReservationAtomicity(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	err := l.AddCompartments(
		CompartmentConfig{ID: "A", Size: SizeMedium},
		CompartmentConfig{ID: "B", Size: SizeLarge},
		CompartmentConfig{ID: "C", Size: SizeSmall},
	)
	if err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	// 中号包裹需要 3 个中号以上格口，但只有 A、B 两个 -> 整笔失败。
	if _, err := l.Reserve(ReservationRequest{Size: SizeMedium, Count: 3}); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("expected ErrInsufficientCapacity, got %v", err)
	}
	if got := l.Status().Available; got != 3 {
		t.Fatalf("failed reservation must not occupy compartments, available=%d", got)
	}

	// 2 个中号格口整笔成功（best-fit：A、B）。
	res, err := l.Reserve(ReservationRequest{Size: SizeMedium, Count: 2})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(res.CompartmentIDs) != 2 {
		t.Fatalf("expected 2 compartments, got %v", res.CompartmentIDs)
	}
	// 小格口 C 不适合中号包裹，不应被选中。
	for _, id := range res.CompartmentIDs {
		if id == "C" {
			t.Fatal("small compartment must not serve a medium parcel")
		}
	}

	// 已经占掉两个，再申请任何中号都应失败且不影响现状。
	if _, err := l.Reserve(ReservationRequest{Size: SizeSmall}); err != nil {
		t.Fatalf("small parcel should still fit in C: %v", err)
	}
	if _, err := l.Reserve(ReservationRequest{Size: SizeMedium}); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("expected capacity error, got %v", err)
	}
}

// 并发预约同一稀缺格口：只有一个包裹赢，其余失败且没有重复分配。
func TestConcurrentReserveNoDoubleBooking(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "only", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	const n = 64
	var wg sync.WaitGroup
	var success int64
	tokens := make(chan string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := l.Reserve(ReservationRequest{Size: SizeMedium})
			if err == nil {
				atomic.AddInt64(&success, 1)
				tokens <- res.DeliveryToken
			} else if !errors.Is(err, ErrInsufficientCapacity) {
				t.Errorf("unexpected reserve error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(tokens)

	if success != 1 {
		t.Fatalf("exactly one reservation must succeed, got %d", success)
	}
	if l.Status().Available != 0 {
		t.Fatal("the only compartment must be occupied")
	}
}

// 并发投递同一凭证：同一内容只成功消费一次（其余幂等返回同结果），
// 变化的内容明确报冲突。
func TestConcurrentDeliverIdempotentAndConflict(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, err := l.Reserve(ReservationRequest{Size: SizeMedium})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	var ok, conflict, other int64
	pickupSeen := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			p := sampleParcel(SizeMedium, "TRK-X")
			if i == n-1 {
				p.Recipient = "someone-else" // 并发的冲突内容
			}
			rec, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: p})
			switch {
			case err == nil:
				atomic.AddInt64(&ok, 1)
				pickupSeen <- rec.PickupToken
			case errors.Is(err, ErrConflict):
				atomic.AddInt64(&conflict, 1)
			default:
				atomic.AddInt64(&other, 1)
				t.Errorf("unexpected deliver error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(pickupSeen)

	if ok+conflict != int64(n) || other != 0 {
		t.Fatalf("ok=%d conflict=%d other=%d", ok, conflict, other)
	}
	if ok == 0 {
		t.Fatal("at least the identical-content deliveries must succeed/replay")
	}
	var first string
	for tk := range pickupSeen {
		if first == "" {
			first = tk
		} else if tk != first {
			t.Fatal("idempotent replay must return the same pickup token")
		}
	}
	// 冲突的那一笔可能恰好先到（成为首投）；无论谁先，最终登记内容必须
	// 唯一，且后续“不同内容”全部拿到冲突。
	st := l.Status()
	var found int
	for _, r := range st.Reservations {
		if r.State == StateDelivered {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one delivered reservation, got %d", found)
	}

	// 投递凭证只能成功消费一次：状态为已投递后，用不同内容再次投递必冲突。
	original := sampleParcel(SizeMedium, "TRK-X")
	changed := original
	changed.WeightGrams = 9999
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: changed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed parcel on replay must conflict, got %v", err)
	}
	// 同内容重放仍返回原取件凭证。
	replay, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: original})
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if replay.PickupToken != first {
		t.Fatal("identical replay must return original pickup token")
	}
}

// 投递凭证的一次性与状态约束：错误凭证、尺寸不符、已取消、已过期。
func TestDeliveryCredentialRules(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock, WithDeliveryTTL(10*time.Minute))
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})

	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: "dlv_not-a-real-token", Parcel: sampleParcel(SizeMedium, "T")}); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("bogus token: %v", err)
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeSmall, "T")}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("size mismatch: %v", err)
	}

	// 取消后不能再投递，格口已释放。
	if _, err := l.Cancel(res.ReservationID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "T")}); !errors.Is(err, ErrReservationCanceled) {
		t.Fatalf("deliver after cancel: %v", err)
	}
	if _, err := l.Cancel(res.ReservationID); !errors.Is(err, ErrReservationCanceled) {
		t.Fatalf("double cancel: %v", err)
	}
	if l.Status().Available != 1 {
		t.Fatal("cancel must release compartment")
	}

	// 投递窗口超时后不能再投递。
	res2, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	clock.advance(11 * time.Minute)
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res2.DeliveryToken, Parcel: sampleParcel(SizeMedium, "T2")}); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("deliver after expiry: %v", err)
	}
	if l.Status().Available != 1 {
		t.Fatal("expired reservation must release compartment")
	}
}

// 取件一次性：重复取件、伪造凭证、取件窗口超时。
func TestPickupOneTime(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock, WithPickupTTL(time.Hour))
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	del, _ := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-P")})

	if _, err := l.Pickup("pck_bogus"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("bogus pickup token: %v", err)
	}
	if _, err := l.Pickup(del.PickupToken); err != nil {
		t.Fatalf("first pickup: %v", err)
	}
	if _, err := l.Pickup(del.PickupToken); !errors.Is(err, ErrAlreadyPickedUp) {
		t.Fatalf("second pickup: %v", err)
	}
	if l.Status().Available != 1 {
		t.Fatal("compartment must be free after pickup")
	}

	// 取件窗口超时。
	res2, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	del2, _ := l.Deliver(DeliveryRequest{DeliveryToken: res2.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-P2")})
	clock.advance(2 * time.Hour)
	if _, err := l.Pickup(del2.PickupToken); !errors.Is(err, ErrCredentialExpired) {
		t.Fatalf("pickup after pickup-window expiry: %v", err)
	}
	if l.Status().Available != 1 {
		t.Fatal("pickup expiry must release compartment")
	}
}

// 取件、取消、超时并发到达：格口只释放一次，终态唯一确定。
func TestConcurrentPickupCancelExpireReleaseOnce(t *testing.T) {
	t.Run("pickup_vs_cancel", func(t *testing.T) {
		testRace(t, func(clock *fakeClock) {}, func(l *Locker, r *ReservationReceipt, d *DeliveryReceipt) error {
			_, err := l.Pickup(d.PickupToken)
			return err
		}, func(l *Locker, r *ReservationReceipt, d *DeliveryReceipt) error {
			_, err := l.Cancel(r.ReservationID)
			return err
		})
	})

	t.Run("pickup_vs_expire", func(t *testing.T) {
		testRace(t, func(clock *fakeClock) { clock.advance(25 * time.Hour) },
			func(l *Locker, r *ReservationReceipt, d *DeliveryReceipt) error {
				_, err := l.Pickup(d.PickupToken)
				return err
			},
			func(l *Locker, r *ReservationReceipt, d *DeliveryReceipt) error {
				_ = l.SweepExpired()
				_, err := l.Pickup(d.PickupToken) // 过期后旧取件请求
				return err
			},
		)
	})

	t.Run("cancel_vs_expire_reserved", func(t *testing.T) {
		clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
		l := newTestLocker(t, clock)
		if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
			t.Fatalf("AddCompartments: %v", err)
		}
		r, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
		clock.advance(11 * time.Minute)

		const n = 24
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					_, _ = l.Cancel(r.ReservationID)
				} else {
					_ = l.SweepExpired()
				}
			}(i)
		}
		close(start)
		wg.Wait()

		st, err := l.Reservation(r.ReservationID)
		if err != nil {
			t.Fatalf("Reservation: %v", err)
		}
		if st.State != StateCanceled && st.State != StateExpired {
			t.Fatalf("terminal state expected, got %q", st.State)
		}
		assertExactlyOneFree(t, l, "X")
	})
}

type raceAction func(l *Locker, r *ReservationReceipt, d *DeliveryReceipt) error

func testRace(t *testing.T, preClock func(clock *fakeClock), a, b raceAction) {
	t.Helper()
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock, WithDeliveryTTL(10*time.Minute), WithPickupTTL(24*time.Hour))
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	r, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	d, err := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-R")})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	preClock(clock)

	const rounds = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _ = a(l, r, d) }()
		go func() { defer wg.Done(); <-start; _ = b(l, r, d) }()
	}
	close(start)
	wg.Wait()

	st, err := l.Reservation(r.ReservationID)
	if err != nil {
		t.Fatalf("Reservation: %v", err)
	}
	if !st.State.Terminal() {
		t.Fatalf("state must be terminal after race, got %q", st.State)
	}
	assertExactlyOneFree(t, l, "X")
}

func assertExactlyOneFree(t *testing.T, l *Locker, id string) {
	t.Helper()
	st := l.Status()
	if st.Available != 1 {
		t.Fatalf("compartment released more than once (available=%d)", st.Available)
	}
	for _, c := range st.Compartments {
		if c.ID == id && c.Occupied {
			t.Fatal("compartment must be free")
		}
	}
}

// 格口复用后，旧取件凭证不能影响后来的包裹。
func TestOldPickupTokenCannotAffectNewParcel(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	// 第一个包裹走完生命周期。
	r1, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	d1, _ := l.Deliver(DeliveryRequest{DeliveryToken: r1.DeliveryToken, Parcel: sampleParcel(SizeMedium, "OLD")})
	oldPickup := d1.PickupToken
	if _, err := l.Pickup(oldPickup); err != nil {
		t.Fatalf("first pickup: %v", err)
	}

	// 同一格口被新预约复用并投递第二个包裹。
	r2, err := l.Reserve(ReservationRequest{Size: SizeMedium})
	if err != nil {
		t.Fatalf("reuse reserve: %v", err)
	}
	if got := r2.CompartmentIDs; len(got) != 1 || got[0] != "X" {
		t.Fatalf("new reservation should reuse X, got %v", got)
	}
	d2, err := l.Deliver(DeliveryRequest{DeliveryToken: r2.DeliveryToken, Parcel: sampleParcel(SizeMedium, "NEW")})
	if err != nil {
		t.Fatalf("second deliver: %v", err)
	}

	// 旧取件凭证：只能得到“已取件/无效”之类的终结答复，绝不释放格口，
	// 新包裹仍在柜中。
	if _, err := l.Pickup(oldPickup); !errors.Is(err, ErrAlreadyPickedUp) {
		t.Fatalf("consumed pickup token replay must report already-picked-up, got %v", err)
	}
	if _, err := l.Pickup("pck_totally-fabricated"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("fabricated pickup token must be invalid, got %v", err)
	}
	st, _ := l.Reservation(r2.ReservationID)
	if st.State != StateDelivered {
		t.Fatalf("new parcel must remain delivered, got %q", st.State)
	}
	if l.Status().Available != 0 {
		t.Fatal("old token must not free the compartment")
	}

	// 新凭证正常取件。
	if _, err := l.Pickup(d2.PickupToken); err != nil {
		t.Fatalf("new pickup: %v", err)
	}
	if l.Status().Available != 1 {
		t.Fatal("new pickup releases compartment")
	}
}

// 取消/过期后格口可被新预约复用，旧投递凭证不再有效。
func TestReuseAfterCancelAndExpiry(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	r1, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	_, _ = l.Cancel(r1.ReservationID)

	r2, err := l.Reserve(ReservationRequest{Size: SizeMedium})
	if err != nil {
		t.Fatalf("reserve after cancel: %v", err)
	}
	if r2.ReservationID == r1.ReservationID {
		t.Fatal("new reservation must get a fresh id")
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: r1.DeliveryToken, Parcel: sampleParcel(SizeMedium, "OLD")}); !errors.Is(err, ErrReservationCanceled) {
		t.Fatalf("old delivery token after cancel: %v", err)
	}

	// 过期路径同样可复用。
	clock.advance(11 * time.Minute)
	if l.Status().Available != 1 {
		t.Fatal("r2 should expire and free X")
	}
	r3, err := l.Reserve(ReservationRequest{Size: SizeMedium})
	if err != nil {
		t.Fatalf("reserve after expiry: %v", err)
	}
	d3, err := l.Deliver(DeliveryRequest{DeliveryToken: r3.DeliveryToken, Parcel: sampleParcel(SizeMedium, "R3")})
	if err != nil {
		t.Fatalf("deliver r3: %v", err)
	}
	if _, err := l.Pickup(d3.PickupToken); err != nil {
		t.Fatalf("pickup r3: %v", err)
	}
}

// 交接记录只含摘要提示，凭证明文与包裹敏感字段不以原文出现在任何记录中。
func TestHandoffsNeverLeakPlaintext(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	r, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	parcel := sampleParcel(SizeMedium, "SECRET-TRACKING")
	d, _ := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: parcel})
	_, _ = l.Pickup(d.PickupToken)

	records := l.Handoffs()
	want := []HandoffEvent{EventReserved, EventDelivered, EventPickedUp}
	if len(records) != len(want) {
		t.Fatalf("expected %d records, got %d", len(want), len(records))
	}
	for i, ev := range want {
		if records[i].Event != ev {
			t.Fatalf("record %d event = %q, want %q", i, records[i].Event, ev)
		}
	}
	for _, rec := range records {
		hay := rec.String()
		for _, secret := range []string{r.DeliveryToken, d.PickupToken, "SECRET-TRACKING"} {
			if strings.Contains(hay, secret) {
				t.Fatalf("handoff record leaks %q: %s", secret, hay)
			}
		}
		if len(rec.DeliveryHint) > hintPrefixLen || len(rec.PickupHint) > hintPrefixLen {
			t.Fatal("hint must be a short prefix only")
		}
	}

	// 返回的是副本，外部修改不影响内部审计链。
	records[0].CompartmentIDs[0] = "HACK"
	if l.Handoffs()[0].CompartmentIDs[0] != "X" {
		t.Fatal("Handoffs must return defensive copies")
	}
}

// 冲突拒绝也会写入交接记录（仍无明文）。
func TestHandoffRejectionRecorded(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	_ = l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium})
	r, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: sampleParcel(SizeMedium, "T")}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	changed := sampleParcel(SizeMedium, "T")
	changed.WeightGrams = 4242
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: changed}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	var sawReject bool
	for _, rec := range l.Handoffs() {
		if rec.Event == EventDeliveryRejected && strings.Contains(rec.Reason, "conflict") {
			sawReject = true
		}
	}
	if !sawReject {
		t.Fatal("expected a delivery_rejected handoff record with conflict reason")
	}
}

// 配置校验：重复 ID、非法尺寸、整笔不部分生效。
func TestAddCompartmentsValidation(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty configs: %v", err)
	}
	if err := l.AddCompartments(
		CompartmentConfig{ID: "A", Size: SizeSmall},
		CompartmentConfig{ID: "A", Size: SizeLarge},
	); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate in batch: %v", err)
	}
	if err := l.AddCompartments(CompartmentConfig{ID: "B", Size: 0}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("bad size: %v", err)
	}
	if err := l.AddCompartments(CompartmentConfig{ID: "A", Size: SizeSmall}); err != nil {
		t.Fatalf("first valid add: %v", err)
	}
	if err := l.AddCompartments(CompartmentConfig{ID: "A", Size: SizeSmall}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("duplicate against store: %v", err)
	}
	if got := l.Status().TotalCompartments; got != 1 {
		t.Fatalf("rejected batches must not partially add, total=%d", got)
	}
}

// SweepExpired 同时推进投递超时与取件超时并返回汇总。
func TestSweepExpiredSummary(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock, WithDeliveryTTL(10*time.Minute), WithPickupTTL(time.Hour))
	_ = l.AddCompartments(
		CompartmentConfig{ID: "A", Size: SizeSmall},
		CompartmentConfig{ID: "B", Size: SizeSmall},
	)
	rA, _ := l.Reserve(ReservationRequest{Size: SizeSmall}) // 投递超时
	rB, _ := l.Reserve(ReservationRequest{Size: SizeSmall})
	dB, _ := l.Deliver(DeliveryRequest{DeliveryToken: rB.DeliveryToken, Parcel: sampleParcel(SizeSmall, "B")}) // 取件超时
	_ = dB

	clock.advance(30 * time.Minute) // rA 投递超时；rB 仍在取件窗口内
	res := l.SweepExpired()
	if len(res.ExpiredReservations) != 1 || res.ExpiredReservations[0] != rA.ReservationID {
		t.Fatalf("expected only rA expired, got %+v", res)
	}
	if len(res.ReleasedCompartments) != 1 || res.ReleasedCompartments[0] != "A" {
		t.Fatalf("expected A released, got %v", res.ReleasedCompartments)
	}

	clock.advance(31 * time.Minute) // 跨过 rB 取件窗口
	res = l.SweepExpired()
	if len(res.ExpiredReservations) != 1 || res.ExpiredReservations[0] != rB.ReservationID {
		t.Fatalf("expected rB expired, got %+v", res)
	}
	// 重复推进应无新变化，格口不会被二次“释放”。
	res = l.SweepExpired()
	if len(res.ExpiredReservations) != 0 {
		t.Fatalf("second sweep must be a no-op, got %+v", res)
	}
	if st := l.Status(); st.Available != 2 {
		t.Fatalf("both compartments free, available=%d", st.Available)
	}
}

// 高并发混合负载下的不变量：每个格口任一时刻最多一个活跃预约。
func TestConcurrentMixedLoadInvariant(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	configs := make([]CompartmentConfig, 8)
	for i := range configs {
		configs[i] = CompartmentConfig{ID: fmt.Sprintf("C%d", i), Size: SizeSmall}
	}
	if err := l.AddCompartments(configs...); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for k := 0; k < 25; k++ {
				r, err := l.Reserve(ReservationRequest{Size: SizeSmall})
				if err != nil {
					continue
				}
				p := sampleParcel(SizeSmall, fmt.Sprintf("W%d-K%d", w, k))
				d, err := l.Deliver(DeliveryRequest{DeliveryToken: r.DeliveryToken, Parcel: p})
				if err != nil {
					_, _ = l.Cancel(r.ReservationID)
					continue
				}
				if _, err := l.Pickup(d.PickupToken); err != nil {
					t.Errorf("pickup w=%d k=%d: %v", w, k, err)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	st := l.Status()
	if st.Available != len(configs) {
		t.Fatalf("all compartments should be free after drain, available=%d", st.Available)
	}
	// 交叉核对：活跃预约占用的格口互不重叠。
	seen := map[string]string{}
	for _, r := range st.Reservations {
		if r.State != StateReserved && r.State != StateDelivered {
			continue
		}
		for _, cid := range r.CompartmentIDs {
			if owner, dup := seen[cid]; dup {
				t.Fatalf("compartment %s assigned to both %s and %s", cid, owner, r.ReservationID)
			}
			seen[cid] = r.ReservationID
		}
	}
}
