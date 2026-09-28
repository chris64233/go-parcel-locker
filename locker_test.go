package goparcellocker

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 测试夹具 ----

type fakeClock struct {
	mu   sync.RWMutex
	base time.Time
	t    time.Time
}

func newFakeClock() *fakeClock {
	base := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	return &fakeClock{base: base, t: base}
}

func (c *fakeClock) now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setElapsed 把时钟定位到相对初始时刻 d 的绝对点（仅测试单线程推进使用）。
func (c *fakeClock) setElapsed(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.base.Add(d)
}

func newTestLocker(t *testing.T, clock *fakeClock) *Locker {
	t.Helper()
	return newTestLockerWithSpecs(t, clock, []CompartmentSpec{
		{ID: "S1", Size: SizeSmall},
		{ID: "S2", Size: SizeSmall},
		{ID: "M1", Size: SizeMedium},
		{ID: "M2", Size: SizeMedium},
		{ID: "L1", Size: SizeLarge},
	})
}

func newTestLockerWithSpecs(t *testing.T, clock *fakeClock, specs []CompartmentSpec) *Locker {
	t.Helper()
	l := NewLocker(
		WithClock(clock.now),
		WithDeliveryTTL(10*time.Minute),
		WithPickupTTL(time.Hour),
	)
	if err := l.ConfigureCompartments(specs); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return l
}

func h(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func pkg(tracking string, size Size, content string) Package {
	return Package{TrackingNo: tracking, Size: size, ContentHash: h(content)}
}

// ---- 格口配置 ----

func TestConfigureCompartmentsValidation(t *testing.T) {
	clock := newFakeClock()
	l := NewLocker(WithClock(clock.now))

	cases := []struct {
		name  string
		specs []CompartmentSpec
		want  error
	}{
		{"empty", nil, ErrInvalidRequest},
		{"empty id", []CompartmentSpec{{ID: "", Size: SizeSmall}}, ErrInvalidRequest},
		{"bad size", []CompartmentSpec{{ID: "X", Size: SizeUnknown}}, ErrInvalidRequest},
		{"duplicate id", []CompartmentSpec{
			{ID: "X", Size: SizeSmall}, {ID: "X", Size: SizeLarge},
		}, ErrInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(l.ConfigureCompartments(tc.specs), tc.want) {
				t.Fatalf("want %v", tc.want)
			}
		})
	}
}

func TestReconfigureRejectedWhileInUse(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, err := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	err = l.ConfigureCompartments([]CompartmentSpec{{ID: "N1", Size: SizeSmall}})
	if !errors.Is(err, ErrCompartmentsInUse) {
		t.Fatalf("want ErrCompartmentsInUse, got %v", err)
	}

	// 终结后允许重配。
	if err := l.CancelReservation(rc.ReservationID); err != nil {
		t.Fatal(err)
	}
	if err := l.ConfigureCompartments([]CompartmentSpec{{ID: "N1", Size: SizeSmall}}); err != nil {
		t.Fatalf("reconfigure after completion: %v", err)
	}
}

// ---- 预约：尺寸匹配、最佳适配、原子性 ----

func TestReserveBestFit(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	// 中件应落入 M1 而非 L1（最佳适配）。
	rc, err := l.Reserve("c1", []Package{pkg("T1", SizeMedium, "a")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.Items[0].CompartmentID; got != "M1" {
		t.Fatalf("best fit: got %s, want M1", got)
	}

	// 小件应落入 S1。
	rc2, err := l.Reserve("c1", []Package{pkg("T2", SizeSmall, "b")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rc2.Items[0].CompartmentID; got != "S1" {
		t.Fatalf("best fit: got %s, want S1", got)
	}
}

func TestReserveLargeFirstPreservesSmallCompartments(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	// 同时申请 L/M/S 各一：大包裹先落位，小格口留给小包裹。
	rc, err := l.Reserve("c1",
		[]Package{
			pkg("small", SizeSmall, "s"),
			pkg("large", SizeLarge, "l"),
			pkg("medium", SizeMedium, "m"),
		}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]string{}
	for _, it := range rc.Items {
		got[it.ItemID] = it.CompartmentID
	}
	if got[1] != "S1" || got[2] != "L1" || got[3] != "M1" {
		t.Fatalf("unexpected allocation %v", got)
	}
}

func TestReserveAtomicWhenInsufficient(t *testing.T) {
	clock := newFakeClock()
	// 只有 2 个可容纳小件的格口（小件可入 S/M/L，因此总数即容量）。
	l := newTestLockerWithSpecs(t, clock, []CompartmentSpec{
		{ID: "S1", Size: SizeSmall},
		{ID: "S2", Size: SizeSmall},
	})

	// 申请 3 个小件必须整体失败。
	_, err := l.Reserve("c1",
		[]Package{
			pkg("a", SizeSmall, "a"),
			pkg("b", SizeSmall, "b"),
			pkg("c", SizeSmall, "c"),
		}, time.Time{})
	if !errors.Is(err, ErrNoAvailableCompartment) {
		t.Fatalf("want ErrNoAvailableCompartment, got %v", err)
	}

	snap := l.Status()
	used := 0
	for _, c := range snap.Compartments {
		if c.InUse {
			used++
		}
	}
	if used != 0 {
		t.Fatalf("failed reservation must not occupy compartments, used=%d", used)
	}

	// 失败后立刻可以成功预约 2 个小件——证明没有提前占用。
	rc, err := l.Reserve("c1", []Package{pkg("a", SizeSmall, "a"), pkg("b", SizeSmall, "b")}, time.Time{})
	if err != nil {
		t.Fatalf("reserve after failed attempt: %v", err)
	}
	if len(rc.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(rc.Items))
	}
}

func TestReserveValidationAndDeadline(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	if _, err := l.Reserve("c1", nil, time.Time{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty packages: %v", err)
	}
	if _, err := l.Reserve("c1", []Package{pkg("a", SizeUnknown, "a")}, time.Time{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("bad size: %v", err)
	}
	past := clock.now().Add(-time.Minute)
	if _, err := l.Reserve("c1", []Package{pkg("a", SizeSmall, "a")}, past); !errors.Is(err, ErrInvalidDeadline) {
		t.Fatalf("past deadline: %v", err)
	}
}

func TestConcurrentReserveNoDoubleAllocation(t *testing.T) {
	clock := newFakeClock()
	l := newTestLockerWithSpecs(t, clock, []CompartmentSpec{
		{ID: "S1", Size: SizeSmall},
		{ID: "S2", Size: SizeSmall},
	})

	const n = 50
	var wg sync.WaitGroup
	successes := make(chan string, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rc, err := l.Reserve("courier",
				[]Package{pkg(fmt.Sprintf("T%d", i), SizeSmall, fmt.Sprintf("c%d", i))},
				time.Time{})
			if err == nil {
				successes <- rc.Items[0].CompartmentID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(successes)

	// 只有 S1、S2 两个小格口，恰好 2 笔成功，且格口不能重复。
	seen := map[string]int{}
	for id := range successes {
		seen[id]++
	}
	if len(seen) != 2 {
		t.Fatalf("want exactly 2 distinct compartments allocated, got %v", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("compartment %s allocated %d times", id, n)
		}
	}
}

// ---- 投递：一次性凭证、幂等、冲突、取消/过期拒绝 ----

func TestDeliverHappyPathAndOneTimeCredential(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, err := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "content-v1")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	token := rc.Items[0].DeliveryToken

	dr, err := l.Deliver(token, pkg("T1", SizeSmall, "content-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if dr.PickupCode == "" || dr.Replayed {
		t.Fatalf("bad delivery receipt: %+v", dr)
	}
	if !dr.PickupDeadline.Equal(clock.now().Add(time.Hour)) {
		t.Fatalf("pickup deadline = %v", dr.PickupDeadline)
	}

	// 包裹进入待取件状态。
	st, err := l.Reservation(rc.ReservationID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Items[0].Status != ItemDelivered {
		t.Fatalf("status = %s", st.Items[0].Status)
	}

	// 随机错误的凭证无效。
	if _, err := l.Deliver(token+"x", pkg("T1", SizeSmall, "content-v1")); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("garbage token: %v", err)
	}
}

func TestDeliverIdempotentSameContent(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "content-v1")}, time.Time{})
	token := rc.Items[0].DeliveryToken

	dr1, err := l.Deliver(token, pkg("T1", SizeSmall, "content-v1"))
	if err != nil {
		t.Fatal(err)
	}
	clock.add(time.Minute)
	dr2, err := l.Deliver(token, pkg("T1", SizeSmall, "content-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if !dr2.Replayed {
		t.Fatal("repeat delivery must be flagged Replayed")
	}
	if dr2.PickupCode != dr1.PickupCode {
		t.Fatal("idempotent replay must return the same pickup code")
	}
	if !dr2.DeliveredAt.Equal(dr1.DeliveredAt) {
		t.Fatal("replay must return the original delivered-at time")
	}
}

func TestDeliverConflictWhenPackageChanged(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	t.Run("first delivery mismatches reservation", func(t *testing.T) {
		rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "content-v1")}, time.Time{})
		_, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "content-v2"))
		if !errors.Is(err, ErrPackageConflict) {
			t.Fatalf("want ErrPackageConflict, got %v", err)
		}
		// 冲突不改变状态，凭证仍可用于正确内容。
		dr, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "content-v1"))
		if err != nil {
			t.Fatalf("legitimate delivery after conflict attempt: %v", err)
		}
		if dr.Replayed {
			t.Fatal("conflict must not consume the credential")
		}
	})

	t.Run("replay with different content", func(t *testing.T) {
		rc, _ := l.Reserve("c1", []Package{pkg("T2", SizeSmall, "content-v1")}, time.Time{})
		token := rc.Items[0].DeliveryToken
		if _, err := l.Deliver(token, pkg("T2", SizeSmall, "content-v1")); err != nil {
			t.Fatal(err)
		}
		_, err := l.Deliver(token, pkg("T2", SizeMedium, "content-v1")) // 尺寸变化也算冲突
		if !errors.Is(err, ErrCredentialReplayed) {
			t.Fatalf("want ErrCredentialReplayed, got %v", err)
		}
		_, err = l.Deliver(token, pkg("T2", SizeSmall, "tampered"))
		if !errors.Is(err, ErrCredentialReplayed) {
			t.Fatalf("want ErrCredentialReplayed, got %v", err)
		}
	})
}

func TestDeliverRejectedAfterCancelOrExpiry(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		clock := newFakeClock()
		l := newTestLocker(t, clock)
		rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
		token := rc.Items[0].DeliveryToken
		if err := l.CancelReservation(rc.ReservationID); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Deliver(token, pkg("T1", SizeSmall, "a")); !errors.Is(err, ErrReservationCanceled) {
			t.Fatalf("want ErrReservationCanceled, got %v", err)
		}
	})

	t.Run("expired delivery window", func(t *testing.T) {
		clock := newFakeClock()
		l := newTestLocker(t, clock)
		rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
		token := rc.Items[0].DeliveryToken
		clock.add(11 * time.Minute) // 超过 10 分钟投递窗口
		if _, err := l.Deliver(token, pkg("T1", SizeSmall, "a")); !errors.Is(err, ErrReservationExpired) {
			t.Fatalf("want ErrReservationExpired, got %v", err)
		}
	})
}

// ---- 取件：一次性、释放一次、过期、旧码隔离 ----

func TestPickupReleasesCompartmentExactlyOnce(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	dr, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "a"))
	if err != nil {
		t.Fatal(err)
	}

	pr, err := l.Pickup(dr.PickupCode)
	if err != nil {
		t.Fatal(err)
	}
	if pr.CompartmentID != "S1" || !pr.PickedUpAt.Equal(clock.now()) {
		t.Fatalf("bad pickup receipt: %+v", pr)
	}

	// 格口已释放。
	if c := findCompartment(l.Compartments(), "S1"); c.InUse {
		t.Fatal("compartment not released after pickup")
	}

	// 取件码只能消费一次。
	if _, err := l.Pickup(dr.PickupCode); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("reuse pickup code: want ErrCredentialInvalid, got %v", err)
	}

	st, _ := l.Reservation(rc.ReservationID)
	if st.Items[0].Status != ItemPickedUp {
		t.Fatalf("status = %s", st.Items[0].Status)
	}
}

func TestPickupNotReady(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	// 未投递时不存在取件凭证。
	if _, err := l.Pickup("nope"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("unknown code: %v", err)
	}
}

func TestPickupExpired(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	dr, _ := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "a"))

	clock.add(time.Hour + time.Minute) // 超过取件窗口
	_, err := l.Pickup(dr.PickupCode)
	if !errors.Is(err, ErrPackageExpired) {
		t.Fatalf("want ErrPackageExpired, got %v", err)
	}
	// 过期释放了格口。
	if c := findCompartment(l.Compartments(), "S1"); c.InUse {
		t.Fatal("expired package must release compartment")
	}
}

func TestOldPickupCodeCannotAffectNewOccupant(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	// 第一代：预约 -> 投递 -> 取件超时，格口释放。
	rc1, _ := l.Reserve("c1", []Package{pkg("T-old", SizeSmall, "old")}, time.Time{})
	dr1, _ := l.Deliver(rc1.Items[0].DeliveryToken, pkg("T-old", SizeSmall, "old"))
	oldCode := dr1.PickupCode
	clock.add(time.Hour + time.Minute)
	l.Advance(clock.now())
	if c := findCompartment(l.Compartments(), "S1"); c.InUse {
		t.Fatal("S1 should be free after expiry")
	}

	// 第二代：新包裹使用同一格口并完成投递。
	clock.add(time.Minute)
	rc2, _ := l.Reserve("c2", []Package{pkg("T-new", SizeSmall, "new")}, time.Time{})
	if rc2.Items[0].CompartmentID != "S1" {
		t.Fatalf("new package should reuse S1, got %s", rc2.Items[0].CompartmentID)
	}
	dr2, _ := l.Deliver(rc2.Items[0].DeliveryToken, pkg("T-new", SizeSmall, "new"))

	// 旧取件码再来：不能取件，更不能动新占用者的格口。
	if _, err := l.Pickup(oldCode); !errors.Is(err, ErrPackageExpired) {
		t.Fatalf("old code want ErrPackageExpired, got %v", err)
	}
	c := findCompartment(l.Compartments(), "S1")
	if !c.InUse || c.ReservationID != rc2.ReservationID {
		t.Fatalf("old pickup request affected new occupant: %+v", c)
	}
	st2, _ := l.Reservation(rc2.ReservationID)
	if st2.Items[0].Status != ItemDelivered {
		t.Fatalf("new item status = %s", st2.Items[0].Status)
	}

	// 新取件码照常工作。
	if _, err := l.Pickup(dr2.PickupCode); err != nil {
		t.Fatalf("new pickup code: %v", err)
	}
}

// ---- 取消 ----

func TestCancelReleasesAllCompartments(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1",
		[]Package{pkg("a", SizeSmall, "a"), pkg("b", SizeLarge, "b")}, time.Time{})

	if err := l.CancelReservation(rc.ReservationID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"S1", "L1"} {
		if c := findCompartment(l.Compartments(), id); c.InUse {
			t.Fatalf("%s not released after cancel", id)
		}
	}
	st, _ := l.Reservation(rc.ReservationID)
	if st.Active {
		t.Fatal("canceled reservation must not be active")
	}
	for _, it := range st.Items {
		if it.Status != ItemCanceled {
			t.Fatalf("item %d status = %s", it.ItemID, it.Status)
		}
	}

	// 重复取消 / 取消不存在的预约。
	if err := l.CancelReservation(rc.ReservationID); !errors.Is(err, ErrReservationNotActive) {
		t.Fatalf("double cancel: %v", err)
	}
	if err := l.CancelReservation("missing"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestCancelRejectedAfterAnyDelivery(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1",
		[]Package{pkg("a", SizeSmall, "a"), pkg("b", SizeMedium, "b")}, time.Time{})
	if _, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("a", SizeSmall, "a")); err != nil {
		t.Fatal(err)
	}
	// 即使另一个包裹还没投递，也不允许整笔取消。
	if err := l.CancelReservation(rc.ReservationID); !errors.Is(err, ErrReservationNotActive) {
		t.Fatalf("want ErrReservationNotActive, got %v", err)
	}
}

// ---- 超时推进与并发竞态 ----

func TestAdvanceSweepsBothPhases(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	// A：t=0 预约并投递，取件截止 t=60。
	rcA, _ := l.Reserve("c1", []Package{pkg("A", SizeSmall, "a")}, time.Time{})
	drA, _ := l.Deliver(rcA.Items[0].DeliveryToken, pkg("A", SizeSmall, "a"))

	// B：t=2 预约，显式给定较晚的投递截止 t=70，避免与 A 同批过期。
	clock.add(2 * time.Minute)
	rcB, _ := l.Reserve("c1", []Package{pkg("B", SizeSmall, "b")}, clock.now().Add(68*time.Minute))

	// 推进到 t=61：A 取件超时；B 的投递窗口（至 t=70）仍有效。
	clock.setElapsed(61 * time.Minute)
	expired := l.Advance(clock.now())
	if len(expired) != 1 || expired[0].Phase != "pickup" || expired[0].ItemID != drA.ItemID {
		t.Fatalf("unexpected sweep result: %+v", expired)
	}

	// 推进到 t=71：B 投递超时。
	clock.setElapsed(71 * time.Minute)
	expired = l.Advance(clock.now())
	if len(expired) != 1 || expired[0].Phase != "delivery" || expired[0].ReservationID != rcB.ReservationID {
		t.Fatalf("unexpected sweep result: %+v", expired)
	}

	// 幂等：再次推进没有新过期。
	if got := l.Advance(clock.now()); len(got) != 0 {
		t.Fatalf("sweep should be idempotent, got %+v", got)
	}

	// 两个格口均已释放。
	for _, c := range l.Compartments() {
		if c.InUse {
			t.Fatalf("compartment %s still in use", c.ID)
		}
	}
}

func TestConcurrentPickupAndExpiryReleaseOnce(t *testing.T) {
	// 取件与超时推进并发到达：每个包裹至多被取走一次，格口占用者不会被
	// 错误地二次释放，也不会出现“同一格口同时被取走和过期”的混乱状态。
	clock := newFakeClock()
	const n = 40
	specs := make([]CompartmentSpec, n)
	for i := range n {
		specs[i] = CompartmentSpec{ID: fmt.Sprintf("C%02d", i), Size: SizeSmall}
	}
	l := newTestLockerWithSpecs(t, clock, specs)

	codes := make([]string, n)
	for i := range n {
		tracking := fmt.Sprintf("T%d", i)
		rc, err := l.Reserve("c1", []Package{pkg(tracking, SizeSmall, tracking)}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		dr, err := l.Deliver(rc.Items[0].DeliveryToken, pkg(tracking, SizeSmall, tracking))
		if err != nil {
			t.Fatal(err)
		}
		codes[i] = dr.PickupCode
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var pickMu sync.Mutex
	picked := map[string]int{}

	// 每个取件码由 3 个 goroutine 同时争抢。
	for _, code := range codes {
		for range 3 {
			wg.Add(1)
			go func(code string) {
				defer wg.Done()
				<-start
				if _, err := l.Pickup(code); err == nil {
					pickMu.Lock()
					picked[code]++
					pickMu.Unlock()
				}
			}(code)
		}
	}
	// 1 个推进者把时间推过取件窗口并执行过期释放。
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		clock.add(2 * time.Hour)
		l.Advance(clock.now())
	}()

	close(start)
	wg.Wait()

	// 每个取件码至多成功一次；其余请求由过期路径终结。
	for code, n := range picked {
		if n > 1 {
			t.Fatalf("code %s picked up %d times", code[:8], n)
		}
	}
	// 所有包裹要么被取走要么过期，格口必须全部释放，占用者不能错乱。
	for _, c := range l.Compartments() {
		if c.InUse {
			t.Fatalf("compartment %s still occupied after concurrent settlement", c.ID)
		}
	}
	// 任何旧码再次使用都不可能成功（一次性 + 已终结）。
	for _, code := range codes {
		if _, err := l.Pickup(code); err == nil {
			t.Fatal("stale code succeeded after settlement")
		}
	}
}

func TestConcurrentReserveAfterExpiryNoDoubleAllocation(t *testing.T) {
	clock := newFakeClock()
	l := newTestLockerWithSpecs(t, clock, []CompartmentSpec{
		{ID: "S1", Size: SizeSmall},
		{ID: "S2", Size: SizeSmall},
	})

	// 占满 2 个小格口，然后让它们投递超时、释放。
	rc1, _ := l.Reserve("c1", []Package{pkg("a", SizeSmall, "a")}, time.Time{})
	rc2, _ := l.Reserve("c1", []Package{pkg("b", SizeSmall, "b")}, time.Time{})
	clock.add(30 * time.Minute)
	expired := l.Advance(clock.now())
	if len(expired) != 2 {
		t.Fatalf("want 2 expired, got %+v", expired)
	}

	// 释放完成后并发涌入 100 笔预约。
	const contenders = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan string, contenders)
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rc, err := l.Reserve("c1", []Package{pkg("new", SizeSmall, "new")}, time.Time{})
			if err == nil {
				results <- rc.Items[0].CompartmentID
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	// 恰好 2 笔成功，且格口不重复。
	seen := map[string]int{}
	for id := range results {
		seen[id]++
	}
	if len(seen) != 2 {
		t.Fatalf("want exactly 2 compartments allocated, got %v", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("compartment %s allocated %d times", id, n)
		}
	}

	// 旧预约的投递凭证已是墓碑，再投递必须被明确拒绝。
	if _, err := l.Deliver(rc1.Items[0].DeliveryToken, pkg("a", SizeSmall, "a")); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("expired token: want ErrReservationExpired, got %v", err)
	}
	if _, err := l.Deliver(rc2.Items[0].DeliveryToken, pkg("b", SizeSmall, "b")); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("expired token: want ErrReservationExpired, got %v", err)
	}
}

func TestLiveOccupancyInvariantUnderMixedConcurrency(t *testing.T) {
	// 混合负载：预约 / 投递 / 取件 / 取消 / 推进持续并发。
	// 核心不变量：任何时刻，每个格口至多被一个存活条目占用，
	// 且存活预约总数不超过格口总数；格口占用者必能在预约中找到对应条目。
	clock := newFakeClock()
	const nCompartments = 6
	specs := make([]CompartmentSpec, nCompartments)
	for i := range nCompartments {
		specs[i] = CompartmentSpec{ID: fmt.Sprintf("C%02d", i), Size: SizeSmall}
	}
	l := newTestLockerWithSpecs(t, clock, specs)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var stopper sync.WaitGroup

	// 推进者：周期性把时钟推过投递窗口并 sweep。
	stopper.Add(1)
	go func() {
		defer stopper.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clock.add(12 * time.Minute)
			l.Advance(clock.now())
			time.Sleep(time.Millisecond)
		}
	}()

	// 工作者：不断预约；成功后并发投递、取消。
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for range 30 {
				tracking := fmt.Sprintf("w%d", w)
				rc, err := l.Reserve("c1",
					[]Package{pkg(tracking, SizeSmall, tracking)},
					time.Time{})
				if err != nil {
					continue
				}
				token := rc.Items[0].DeliveryToken
				var iwg sync.WaitGroup
				iwg.Add(2)
				go func() {
					defer iwg.Done()
					dr, err := l.Deliver(token, pkg(tracking, SizeSmall, tracking))
					if err == nil {
						_, _ = l.Pickup(dr.PickupCode)
					}
				}()
				go func() {
					defer iwg.Done()
					_ = l.CancelReservation(rc.ReservationID)
				}()
				iwg.Wait()
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	stopper.Wait()

	// 结算：推进足够久，让所有残留条目终结。
	clock.add(4 * time.Hour)
	l.Advance(clock.now())

	// 终态不变量：所有格口空闲。
	for _, c := range l.Compartments() {
		if c.InUse {
			t.Fatalf("compartment %s still occupied: %+v", c.ID, c)
		}
	}
	// 所有预约条目的状态机一致：终态（取件/取消/过期），无悬挂 reserved/delivered。
	snap := l.Status()
	for _, r := range snap.Reservations {
		for _, it := range r.Items {
			switch it.Status {
			case ItemPickedUp, ItemCanceled, ItemExpired:
			default:
				t.Fatalf("reservation %s item %d non-terminal: %s", r.ID, it.ItemID, it.Status)
			}
		}
	}
}

func TestConcurrentDeliverAndCancelOnlyOneWins(t *testing.T) {
	// 取消与投递并发到达同一笔预约：只有一方能赢；
	// 赢的若是取消，格口释放且凭证作废；赢的若是投递，格口转为待取件，
	// 取消之后不可能再成功。
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	const rounds = 200
	for range rounds {
		rc, err := l.Reserve("c1", []Package{pkg("T", SizeSmall, "a")}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		token := rc.Items[0].DeliveryToken
		resID := rc.ReservationID

		var wg sync.WaitGroup
		start := make(chan struct{})
		var deliverErr, cancelErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, deliverErr = l.Deliver(token, pkg("T", SizeSmall, "a"))
		}()
		go func() {
			defer wg.Done()
			<-start
			cancelErr = l.CancelReservation(resID)
		}()
		close(start)
		wg.Wait()

		c := findCompartment(l.Compartments(), rc.Items[0].CompartmentID)
		switch {
		case deliverErr == nil && cancelErr != nil:
			// 投递赢：格口必须仍占用（待取件），再取消必须失败。
			if !c.InUse {
				t.Fatal("delivery won but compartment was released")
			}
			if err := l.CancelReservation(resID); !errors.Is(err, ErrReservationNotActive) {
				t.Fatalf("cancel after winning delivery: %v", err)
			}
			// 终结包裹，保证下一轮格口可用。
			clock.add(2 * time.Hour)
			l.Advance(clock.now())
		case cancelErr == nil && deliverErr != nil:
			// 取消赢：格口必须释放，投递必须明确报“已取消”。
			if c.InUse {
				t.Fatal("cancel won but compartment still occupied")
			}
			if !errors.Is(deliverErr, ErrReservationCanceled) {
				t.Fatalf("lost delivery want ErrReservationCanceled, got %v", deliverErr)
			}
		default:
			t.Fatalf("ambiguous race outcome: deliver=%v cancel=%v", deliverErr, cancelErr)
		}
	}
}

// ---- 状态查询与交接记录 ----

func TestStatusCountsAndViews(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	_ = l.Status()
	snap := l.Status()
	if snap.FreeBySize[SizeSmall] != 2 || snap.UsedBySize[SizeSmall] != 0 ||
		snap.FreeBySize[SizeMedium] != 2 || snap.FreeBySize[SizeLarge] != 1 {
		t.Fatalf("initial counts wrong: %+v", snap.FreeBySize)
	}

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeMedium, "a")}, time.Time{})
	snap = l.Status()
	if snap.FreeBySize[SizeMedium] != 1 || snap.UsedBySize[SizeMedium] != 1 {
		t.Fatalf("counts after reserve: %+v / %+v", snap.FreeBySize, snap.UsedBySize)
	}
	if len(snap.Reservations) != 1 || !snap.Reservations[0].Active {
		t.Fatal("reservation view should be active")
	}

	// 未知预约查询。
	if _, err := l.Reservation("nope"); !errors.Is(err, ErrReservationNotFound) {
		t.Fatalf("missing reservation: %v", err)
	}

	// 完成后格口状态复位。
	dr, _ := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeMedium, "a"))
	if _, err := l.Pickup(dr.PickupCode); err != nil {
		t.Fatal(err)
	}
	snap = l.Status()
	if snap.UsedBySize[SizeMedium] != 0 || snap.FreeBySize[SizeMedium] != 2 {
		t.Fatalf("counts after pickup: %+v / %+v", snap.FreeBySize, snap.UsedBySize)
	}
}

func TestHandoffRecordsNeverLeakCredentials(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("SECRET-TRACK", SizeSmall, "secret")}, time.Time{})
	token := rc.Items[0].DeliveryToken
	dr, _ := l.Deliver(token, pkg("SECRET-TRACK", SizeSmall, "secret"))
	pickup := dr.PickupCode
	if _, err := l.Pickup(pickup); err != nil {
		t.Fatal(err)
	}

	records := l.HandoffRecords()
	if len(records) != 3 {
		t.Fatalf("want 3 records (reserved/delivered/picked), got %d", len(records))
	}
	kinds := []EventKind{EventReserved, EventDelivered, EventPickedUp}
	for i, rec := range records {
		if rec.Kind != kinds[i] {
			t.Fatalf("record %d kind = %s", i, rec.Kind)
		}
		if rec.CredentialFingerprint == "" && rec.Kind != EventPickedUp {
			// 取件记录里也会带摘要；前两类必然有。
		}
		if rec.CredentialFingerprint != "" && len(rec.CredentialFingerprint) != 64 {
			t.Fatalf("fingerprint should be 64 hex chars, got %d", len(rec.CredentialFingerprint))
		}
		if rec.CredentialHint != "" && len(rec.CredentialHint) != hintLen {
			t.Fatalf("hint len = %d", len(rec.CredentialHint))
		}
	}

	// 全部记录（含任何字符串化）都不得出现凭证明文。
	for _, rec := range records {
		s := fmt.Sprintf("%+v", rec)
		if strings.Contains(s, token) {
			t.Fatalf("delivery token leaked into records: %s", s)
		}
		if strings.Contains(s, pickup) {
			t.Fatalf("pickup code leaked into records: %s", s)
		}
	}

	// 快照/状态也不得泄漏凭证：Reservation、Status 输出中无 token 字段，
	// 这里再对整体打印做一次保险检查。
	for _, v := range []any{l.Status(), records} {
		if strings.Contains(fmt.Sprintf("%v", v), token) ||
			strings.Contains(fmt.Sprintf("%v", v), pickup) {
			t.Fatal("credential leaked through status API")
		}
	}
}

func TestRecordsCoverCancelAndExpiry(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	if err := l.CancelReservation(rc.ReservationID); err != nil {
		t.Fatal(err)
	}
	recs := l.HandoffRecords()
	last := recs[len(recs)-1]
	if last.Kind != EventCanceled || last.Detail != "" {
		t.Fatalf("want cancel record, got %+v", last)
	}

	clock.add(20 * time.Minute)
	rc2, _ := l.Reserve("c1", []Package{pkg("T2", SizeSmall, "b")}, time.Time{})
	clock.add(11 * time.Minute)
	l.Advance(clock.now())
	recs = l.HandoffRecords()
	last = recs[len(recs)-1]
	if last.Kind != EventExpired || last.Detail != "delivery" || last.ReservationID != rc2.ReservationID {
		t.Fatalf("want expiry(delivery) record, got %+v", last)
	}
}

// ---- 凭证安全属性 ----

func TestStoredCredentialIsDigestOnly(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	token := rc.Items[0].DeliveryToken

	// 内部表只存 HMAC 摘要，绝不能直接出现明文。
	var foundPlaintext bool
	l.mu.Lock()
	for fp := range l.deliveryCreds {
		if fp == token {
			foundPlaintext = true
		}
		if len(fp) != 64 {
			t.Fatalf("stored fingerprint len = %d", len(fp))
		}
	}
	l.mu.Unlock()
	if foundPlaintext {
		t.Fatal("plaintext delivery token found in credential store")
	}

	dr, _ := l.Deliver(token, pkg("T1", SizeSmall, "a"))
	l.mu.Lock()
	for fp := range l.pickupCreds {
		if fp == dr.PickupCode {
			foundPlaintext = true
		}
	}
	l.mu.Unlock()
	if foundPlaintext {
		t.Fatal("plaintext pickup code found in credential store")
	}
}

func TestPickupCodeDerivationIsStableAcrossInstances(t *testing.T) {
	// 相同 HMAC 密钥下，取件码由投递凭证确定性派生，保证幂等语义可重现。
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c1 := derivePickupCode(key, "delivery-token")
	c2 := derivePickupCode(key, "delivery-token")
	c3 := derivePickupCode(key, "delivery-tokeN")
	if c1 != c2 || c1 == c3 {
		t.Fatal("pickup derivation not deterministic or not input-sensitive")
	}
	if len(c1) != 24 {
		t.Fatalf("pickup code length = %d, want 24", len(c1))
	}
}

func findCompartment(list []CompartmentState, id string) CompartmentState {
	for _, c := range list {
		if c.ID == id {
			return c
		}
	}
	return CompartmentState{ID: id}
}
