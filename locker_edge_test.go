package goparcellocker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 预约请求校验：Count 为 0 按 1 处理；负数数量与非法尺寸整笔拒绝。
func TestReserveRequestValidation(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(
		CompartmentConfig{ID: "A", Size: SizeSmall},
		CompartmentConfig{ID: "B", Size: SizeSmall},
	); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	if _, err := l.Reserve(ReservationRequest{Size: 0}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid size: %v", err)
	}
	if _, err := l.Reserve(ReservationRequest{Size: SizeSmall, Count: -2}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("negative count: %v", err)
	}
	// 非法请求不得占用任何格口。
	if got := l.Status().Available; got != 2 {
		t.Fatalf("invalid requests must not occupy compartments, available=%d", got)
	}

	// Count 缺省（0）按 1 处理。
	res, err := l.Reserve(ReservationRequest{Size: SizeSmall})
	if err != nil {
		t.Fatalf("default count reserve: %v", err)
	}
	if len(res.CompartmentIDs) != 1 {
		t.Fatalf("default count must reserve exactly 1 compartment, got %v", res.CompartmentIDs)
	}
}

// 多格口整笔预约的完整生命周期：一次投递、一次取件，两个格口同时释放。
func TestMultiCompartmentLifecycle(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC))
	l := newTestLocker(t, clock)
	if err := l.AddCompartments(
		CompartmentConfig{ID: "A", Size: SizeMedium},
		CompartmentConfig{ID: "B", Size: SizeMedium},
		CompartmentConfig{ID: "C", Size: SizeMedium},
	); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}

	res, err := l.Reserve(ReservationRequest{Size: SizeMedium, Count: 2})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(res.CompartmentIDs) != 2 {
		t.Fatalf("expected 2 compartments, got %v", res.CompartmentIDs)
	}
	if got := l.Status().Available; got != 1 {
		t.Fatalf("expected 1 available after reserving 2, got %d", got)
	}

	del, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-M")})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(del.CompartmentIDs) != 2 {
		t.Fatalf("delivery receipt must list both compartments, got %v", del.CompartmentIDs)
	}

	pick, err := l.Pickup(del.PickupToken)
	if err != nil {
		t.Fatalf("Pickup: %v", err)
	}
	if len(pick.CompartmentIDs) != 2 {
		t.Fatalf("pickup receipt must list both compartments, got %v", pick.CompartmentIDs)
	}
	if got := l.Status().Available; got != 3 {
		t.Fatalf("both compartments must be released by one pickup, available=%d", got)
	}
}

// 投递凭证在取件完成后彻底失效：同内容重放也返回“已消费”，不会重新投递。
func TestDeliverAfterPickupConsumed(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	parcel := sampleParcel(SizeMedium, "TRK-C")
	del, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: parcel})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := l.Pickup(del.PickupToken); err != nil {
		t.Fatalf("Pickup: %v", err)
	}

	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: parcel}); !errors.Is(err, ErrCredentialConsumed) {
		t.Fatalf("deliver replay after pickup must report consumed, got %v", err)
	}
	st, _ := l.Reservation(res.ReservationID)
	if st.State != StatePickedUp {
		t.Fatalf("reservation must stay picked_up, got %q", st.State)
	}
	if got := l.Status().Available; got != 1 {
		t.Fatalf("consumed replay must not re-occupy the compartment, available=%d", got)
	}
}

// 两类凭证互不通用：取件凭证不能投递，投递凭证不能取件。
func TestCrossCredentialMisuse(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	del, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-X")})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: del.PickupToken, Parcel: sampleParcel(SizeMedium, "TRK-X")}); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("pickup token used as delivery token must be invalid, got %v", err)
	}
	if _, err := l.Pickup(res.DeliveryToken); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("delivery token used as pickup token must be invalid, got %v", err)
	}
	if _, err := l.Pickup(""); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("empty pickup token must be invalid, got %v", err)
	}
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: "", Parcel: sampleParcel(SizeMedium, "TRK-X")}); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("empty delivery token must be invalid, got %v", err)
	}

	// 误用不影响在柜包裹。
	st, _ := l.Reservation(res.ReservationID)
	if st.State != StateDelivered {
		t.Fatalf("reservation must stay delivered, got %q", st.State)
	}
}

// 取消的状态约束：已投递预约取消报冲突；未知预约报不存在。
func TestCancelStateRules(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	if _, err := l.Cancel("no-such-reservation"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown reservation: %v", err)
	}
	if _, err := l.Cancel(""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("cancel empty id: %v", err)
	}

	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	if _, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-C")}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := l.Cancel(res.ReservationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel after delivery must conflict, got %v", err)
	}
	st, _ := l.Reservation(res.ReservationID)
	if st.State != StateDelivered {
		t.Fatalf("reservation must stay delivered after conflicting cancel, got %q", st.State)
	}
	if got := l.Status().Available; got != 0 {
		t.Fatalf("conflicting cancel must not release the compartment, available=%d", got)
	}
}

// 状态查询快照：不含任何凭证明文，且返回的是防御性副本。
func TestStatusSnapshotNoPlaintextAndDefensiveCopy(t *testing.T) {
	l := newTestLocker(t, nil)
	if err := l.AddCompartments(CompartmentConfig{ID: "X", Size: SizeMedium}); err != nil {
		t.Fatalf("AddCompartments: %v", err)
	}
	res, _ := l.Reserve(ReservationRequest{Size: SizeMedium})
	del, err := l.Deliver(DeliveryRequest{DeliveryToken: res.DeliveryToken, Parcel: sampleParcel(SizeMedium, "TRK-S")})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	st := l.Status()
	dump := fmt.Sprintf("%+v", st)
	for _, secret := range []string{res.DeliveryToken, del.PickupToken} {
		if strings.Contains(dump, secret) {
			t.Fatalf("status snapshot leaks credential %q", secret)
		}
	}
	rview, err := l.Reservation(res.ReservationID)
	if err != nil {
		t.Fatalf("Reservation: %v", err)
	}
	if dump := fmt.Sprintf("%+v", rview); strings.Contains(dump, res.DeliveryToken) || strings.Contains(dump, del.PickupToken) {
		t.Fatalf("reservation view leaks credential: %s", dump)
	}
	if _, err := l.Reservation("no-such-reservation"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown reservation: %v", err)
	}

	// 修改返回的快照不影响内部状态。
	st.Compartments[0].Occupied = false
	st.Reservations[0].CompartmentIDs[0] = "HACK"
	st.Reservations[0].Parcel.Recipient = "HACK"
	fresh := l.Status()
	if !fresh.Compartments[0].Occupied {
		t.Fatal("status view must be a defensive copy (compartment)")
	}
	if fresh.Reservations[0].CompartmentIDs[0] != "X" {
		t.Fatal("status view must be a defensive copy (compartment ids)")
	}
	if fresh.Reservations[0].Parcel.Recipient == "HACK" {
		t.Fatal("status view must be a defensive copy (parcel)")
	}
}
