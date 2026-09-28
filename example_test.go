package goparcellocker_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	goparcellocker "github.com/chris64233/go-parcel-locker"
)

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Example 展示完整的“预约 → 投递 → 取件”交接链路。
func Example() {
	// 实际使用时时间源默认为 time.Now；此处注入固定时钟仅为让输出确定。
	base := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	now := base
	locker := goparcellocker.NewLocker(goparcellocker.WithClock(func() time.Time { return now }))

	if err := locker.ConfigureCompartments([]goparcellocker.CompartmentSpec{
		{ID: "S1", Size: goparcellocker.SizeSmall},
		{ID: "M1", Size: goparcellocker.SizeMedium},
	}); err != nil {
		panic(err)
	}

	// 1) 快递员按包裹尺寸申请格口（整笔预约）。
	receipt, err := locker.Reserve("courier-7",
		[]goparcellocker.Package{{TrackingNo: "SF123", Size: goparcellocker.SizeSmall, ContentHash: digest("book")}},
		time.Time{})
	if err != nil {
		panic(err)
	}
	r := receipt.Items[0]
	fmt.Println("reserved at:", r.CompartmentID)

	// 2) 快递员持一次性投递凭证投递；系统返回取件码。
	delivered, err := locker.Deliver(r.DeliveryToken,
		goparcellocker.Package{TrackingNo: "SF123", Size: goparcellocker.SizeSmall, ContentHash: digest("book")})
	if err != nil {
		panic(err)
	}
	fmt.Println("delivered, pickup code issued (len):", len(delivered.PickupCode))

	// 重复投递同一内容：幂等返回原结果，取件码相同。
	replay, err := locker.Deliver(r.DeliveryToken,
		goparcellocker.Package{TrackingNo: "SF123", Size: goparcellocker.SizeSmall, ContentHash: digest("book")})
	if err != nil {
		panic(err)
	}
	fmt.Println("idempotent replay:", replay.Replayed, replay.PickupCode == delivered.PickupCode)

	// 包裹信息变化：明确报冲突。
	_, err = locker.Deliver(r.DeliveryToken,
		goparcellocker.Package{TrackingNo: "SF123", Size: goparcellocker.SizeSmall, ContentHash: digest("laptop")})
	fmt.Println("conflict detected:", errors.Is(err, goparcellocker.ErrCredentialReplayed))

	// 3) 收件人持取件码一次性取件，格口释放。
	picked, err := locker.Pickup(delivered.PickupCode)
	if err != nil {
		panic(err)
	}
	fmt.Println("picked up from:", picked.CompartmentID)

	// 取件码只能用一次。
	_, err = locker.Pickup(delivered.PickupCode)
	fmt.Println("code reused rejected:", errors.Is(err, goparcellocker.ErrCredentialInvalid))

	// 交接记录不含任何凭证明文。
	for _, rec := range locker.HandoffRecords() {
		fmt.Println("record:", rec.Kind, "hint-len:", len(rec.CredentialHint))
	}

	// Output:
	// reserved at: S1
	// delivered, pickup code issued (len): 24
	// idempotent replay: true true
	// conflict detected: true
	// picked up from: S1
	// code reused rejected: true
	// record: reserved hint-len: 12
	// record: delivered hint-len: 12
	// record: picked_up hint-len: 12
}
