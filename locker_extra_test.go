package goparcellocker

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultTTLOptionsAndZeroAdvance(t *testing.T) {
	// 不传 TTL 选项：使用默认投递窗口 15 分钟；Advance(零值) 使用时钟当前值。
	clock := newFakeClock()
	l := NewLocker(WithClock(clock.now))
	if err := l.ConfigureCompartments([]CompartmentSpec{{ID: "S1", Size: SizeSmall}}); err != nil {
		t.Fatal(err)
	}

	rc, err := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !rc.DeliveryDeadline.Equal(clock.now().Add(15 * time.Minute)) {
		t.Fatalf("default delivery deadline = %v", rc.DeliveryDeadline)
	}
	if rc.Items[0].DeliveryExpires != rc.DeliveryDeadline {
		t.Fatal("per-item expiry must equal reservation deadline")
	}

	// Advance 零值参数：取当前时钟（尚未到点，不释放）。
	if got := l.Advance(time.Time{}); len(got) != 0 {
		t.Fatalf("nothing should expire yet: %+v", got)
	}
	clock.add(15 * time.Minute)
	if got := l.Advance(time.Time{}); len(got) != 1 || got[0].Phase != "delivery" {
		t.Fatalf("zero-arg Advance should sweep now, got %+v", got)
	}
	if _, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "a")); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestHMACKeyOptionAndInvalidOptionsIgnored(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	clock := newFakeClock()
	l := NewLocker(
		WithClock(clock.now),
		WithDeliveryTTL(-time.Second), // 非法值被忽略，回落默认
		WithPickupTTL(0),              // 非法值被忽略，回落默认
		WithHMACKey(nil),              // 空密钥被忽略，使用随机
		WithHMACKey(key),              // 显式密钥生效
		WithClock(nil),                // nil 时钟被忽略
	)
	if err := l.ConfigureCompartments([]CompartmentSpec{{ID: "S1", Size: SizeSmall}}); err != nil {
		t.Fatal(err)
	}
	rc, err := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if !rc.DeliveryDeadline.Equal(clock.now().Add(15 * time.Minute)) {
		t.Fatalf("invalid TTL option should be ignored, deadline=%v", rc.DeliveryDeadline)
	}
	dr, err := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if want := derivePickupCode(key, rc.Items[0].DeliveryToken); dr.PickupCode != want {
		t.Fatal("injected HMAC key should drive pickup code derivation")
	}
}

func TestStringRepresentations(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{SizeSmall.String(), "S"},
		{SizeMedium.String(), "M"},
		{SizeLarge.String(), "L"},
		{SizeUnknown.String(), "?"},
		{ItemReserved.String(), "reserved"},
		{ItemDelivered.String(), "delivered"},
		{ItemPickedUp.String(), "picked_up"},
		{ItemCanceled.String(), "canceled"},
		{ItemExpired.String(), "expired"},
		{ItemStatus(99).String(), "unknown"},
		{EventReserved.String(), "reserved"},
		{EventDelivered.String(), "delivered"},
		{EventPickedUp.String(), "picked_up"},
		{EventCanceled.String(), "canceled"},
		{EventExpired.String(), "expired"},
		{EventKind(99).String(), "unknown"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestPickupFromWrongState(t *testing.T) {
	// 取件一个已取消预约对应代际的“伪造码”：直接 ErrCredentialInvalid（墓碑表里无此指纹）。
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	if err := l.CancelReservation(rc.ReservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Pickup("forged-code"); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("want ErrCredentialInvalid, got %v", err)
	}
}

func TestReplayPickupCodeAfterPickupIsInvalid(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, _ := l.Reserve("c1", []Package{pkg("T1", SizeSmall, "a")}, time.Time{})
	dr, _ := l.Deliver(rc.Items[0].DeliveryToken, pkg("T1", SizeSmall, "a"))
	if _, err := l.Pickup(dr.PickupCode); err != nil {
		t.Fatal(err)
	}
	// 取件成功后取件凭证已删除，复用直接无效（而不是返回“已取件”之外的信息）。
	if _, err := l.Pickup(dr.PickupCode); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("reuse: %v", err)
	}
}

func TestReserveMultipleTokensAreUniqueAndSecret(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc, err := l.Reserve("c1",
		[]Package{
			pkg("a", SizeSmall, "a"),
			pkg("b", SizeMedium, "b"),
			pkg("c", SizeLarge, "c"),
			pkg("d", SizeSmall, "d"),
			pkg("e", SizeMedium, "e"),
		}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, it := range rc.Items {
		if it.DeliveryToken == "" {
			t.Fatal("empty token")
		}
		if seen[it.DeliveryToken] {
			t.Fatal("duplicate delivery token in one reservation")
		}
		seen[it.DeliveryToken] = true
	}
}

func TestCancelOneOfTwoReservationsDoesNotAffectOther(t *testing.T) {
	clock := newFakeClock()
	l := newTestLocker(t, clock)

	rc1, _ := l.Reserve("c1", []Package{pkg("A", SizeSmall, "a")}, time.Time{})
	rc2, _ := l.Reserve("c2", []Package{pkg("B", SizeSmall, "b")}, time.Time{})

	if err := l.CancelReservation(rc1.ReservationID); err != nil {
		t.Fatal(err)
	}
	// rc2 不受影响，可以正常投递取件。
	dr, err := l.Deliver(rc2.Items[0].DeliveryToken, pkg("B", SizeSmall, "b"))
	if err != nil {
		t.Fatalf("unrelated reservation affected by cancel: %v", err)
	}
	if _, err := l.Pickup(dr.PickupCode); err != nil {
		t.Fatal(err)
	}
}
