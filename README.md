# go-parcel-locker

并发安全的快递柜格口交接库（Go）：快递员按包裹尺寸**预约**格口、凭短期有效的
**投递凭证**投递、系统生成一次性**取件凭证**，并在取件 / 取消 / 超时并发到达时
保证格口**精确一次释放**。

包名：`goparcellocker`（`github.com/chris64233/go-parcel-locker`），无第三方依赖，
仅使用 Go 标准库。

## 生命周期

```
                 Reserve                Deliver                 Pickup
  (空闲) ───────────────────────▶ reserved ───────────────▶ delivered ───────────────▶ picked_up
                                    │  ▲                        │  ▲                      (格口释放)
                                    │  │                        │  │
                              Cancel│  │DeliveryTTL 过期        │  │PickupTTL 过期
                                    ▼  │                        ▼  │
                                canceled                    expired
                                                            (格口释放)
```

- `reserved`：格口已占用，等待投递；投递凭证默认 10 分钟有效。
- `delivered`：包裹在柜，等待取件；取件凭证默认 24 小时有效。
- `picked_up` / `canceled` / `expired`：终态，格口已归还可用池，可被新预约复用。

## 快速开始

```go
package main

import (
	"fmt"

	locker "github.com/chris64233/go-parcel-locker"
)

func main() {
	l, _ := locker.NewLocker() // 可用 WithDeliveryTTL / WithPickupTTL / WithClock 调整
	if err := l.AddCompartments(
		locker.CompartmentConfig{ID: "A-01", Size: locker.SizeSmall},
		locker.CompartmentConfig{ID: "A-02", Size: locker.SizeLarge},
	); err != nil {
		panic(err)
	}

	// 1) 按尺寸整笔申请；容量不足返回 ErrInsufficientCapacity，且不占用任何格口
	res, err := l.Reserve(locker.ReservationRequest{Size: locker.SizeSmall, Count: 1})
	if err != nil {
		panic(err)
	}
	fmt.Println("投递凭证（仅此一次返回）：", res.DeliveryToken)

	// 2) 在有效期内投递，得到取件凭证
	del, err := l.Deliver(locker.DeliveryRequest{
		DeliveryToken: res.DeliveryToken,
		Parcel: locker.ParcelInfo{
			TrackingNumber: "SF123",
			Size:           locker.SizeSmall,
			WeightGrams:    800,
			Sender:         "Alice",
			Recipient:      "Bob",
		},
	})
	if err != nil {
		panic(err)
	}

	// 3) 一次性取件；成功后格口立即释放，可被复用
	if _, err := l.Pickup(del.PickupToken); err != nil {
		panic(err)
	}
}
```

## API 概览

| 方法 | 说明 |
| --- | --- |
| `NewLocker(...Option)` | 创建快递柜；可注入时钟与两类凭证 TTL |
| `AddCompartments(...CompartmentConfig)` | 配置格口；重复 ID / 非法尺寸整笔拒绝，不部分生效 |
| `Reserve(ReservationRequest)` | 按尺寸 + 数量整笔预约，best-fit 选格，返回投递凭证与到期时间 |
| `Deliver(DeliveryRequest)` | 凭投递凭证登记包裹，返回新的取件凭证；幂等重放 / 冲突判定 |
| `Pickup(token)` | 凭取件凭证取件，成功即释放格口，凭证立即失效 |
| `Cancel(reservationID)` | 投递前取消并释放格口 |
| `SweepExpired()` | 主动推进超时，返回本次过期的预约与释放的格口 |
| `Status()` / `Reservation(id)` | 状态快照查询（先惰性推进超时），不含任何凭证明文 |
| `Handoffs()` | 只追加的交接记录副本，只含凭证摘要前缀，可安全审计 |

## 关键语义与并发保证

1. **预约原子性**。所有状态变更都在同一把互斥锁下完成：只有当“尺寸合适且
   仍可用”的格口数量满足整笔申请时才占用，成功前任何一步失败都不改动状态
   （返回 `ErrInsufficientCapacity`，格口占用数不变）。并发申请同一稀缺格口时，
   恰好一笔成功。选格采用 best-fit（能容纳的最小尺寸，ID 次序兜底），避免
   小包裹占走大格口，分配结果确定。

2. **凭证只存摘要、投递凭证只消费一次**。凭证明文使用 `crypto/rand` 生成
   （256 位随机，`dlv_` / `pck_` 前缀），系统内部仅保存 HMAC-SHA256 摘要，
   明文只在创建时返回一次。
   - 同一投递凭证 + **相同包裹信息**重复投递：幂等返回**原结果与相同的取件凭证**
     （取件凭证由投递凭证确定性派生，因此重放结果稳定）。
   - 同一投递凭证 + **不同包裹信息**：返回 `ErrConflict`，并写入
     `delivery_rejected` 交接记录，首投内容不被覆盖。
   - 预约已取消 / 投递窗口已过期：返回 `ErrReservationCanceled` /
     `ErrCredentialExpired`，不能再投递。

3. **取件 / 取消 / 超时的精确一次释放**。三个入口在同一临界区内做
   “检查状态 → 置终态 → 删凭证索引 → 清格口占用”，终态操作幂等：
   - 取件成功后再次使用同一取件凭证返回 `ErrAlreadyPickedUp`，格口不会被二次释放；
   - 取件与取消、取件与过期、取消与过期并发到达时，恰有一方完成状态迁移，
     另一方得到对应终态错误；
   - 格口归还后可被新预约复用。旧凭证索引已从活跃索引中删除，旧取件请求只会
     得到终态/无效答复，**不可能释放新包裹占用的格口**。

   除状态机约束外，每个格口还记录**当前占用者的预约 ID（fencing token）**：
   释放格口时必须占用身份匹配才生效。即使旧预约的释放路径迟到、且格口已被
   后来的预约复用，旧交接者也无法从数据结构层面动到新占用者——“精确一次释放”
   不依赖操作顺序的推导，而由占用身份直接保证。

4. **超时推进**。`SweepExpired()` 可由定时任务批量回收；各业务方法在加锁后也会
   惰性推进，因此即便没有后台扫描，过期凭证也无法投递或取件。重复推进为空操作。
   `SweepResult` 的过期预约与释放格口列表均按字典序返回，不依赖 map 遍历顺序，
   便于调用方稳定地比较与落盘。

### 错误判定

全部为可 `errors.Is` 判定的哨兵错误：`ErrInvalidRequest`、`ErrInsufficientCapacity`、
`ErrNotFound`、`ErrCredentialInvalid`、`ErrCredentialExpired`、
`ErrCredentialConsumed`、`ErrAlreadyPickedUp`、`ErrReservationCanceled`、
`ErrConflict`。

## 交接记录（不泄漏明文）

`Handoffs()` 返回按时间追加的 `HandoffRecord`：记录事件（预约 / 投递 / 取件 /
取消 / 投递超时 / 取件超时 / 拒绝）、预约与格口 ID，以及凭证摘要的 **12 位十六进制
短前缀**（仅用于人工交叉核对，无法还原凭证）。记录中不保存凭证明文，也不保存
运单号、收件人等包裹原文；返回值是防御性副本，外部修改不影响内部审计链。

## 测试

```sh
go test -race -count=1 ./...
```

`locker_test.go` 覆盖：

- happy path 与多格口整笔预约的成功 / 失败不占格、best-fit 选格；
- 64 路并发预约同一格口恰好一笔成功；
- 32 路并发投递同一凭证：相同内容幂等返回同一取件凭证、不同内容全部冲突；
- 取件 vs 取消、取件 vs 过期、取消 vs 过期的 50 轮并发竞态，断言终态唯一、
  格口只释放一次；
- 格口复用后旧取件凭证不能影响新包裹；取消 / 过期后格口可再预约；
- `SweepExpired` 汇总与重复推进幂等；32 worker 混合负载下“一格口至多一个活跃
  预约”的不变量；
- 格口占用身份（fencing）：预约 / 取消 / 复用 / 取件各阶段 `occupantID`
  正确流转，旧预约的释放路径在格口被复用后不会释放新占用者；批量超时的
  `SweepResult` 列表确定有序，且释放后格口可整笔再预约；
- 四类终态（已取件 / 已取消 / 投递超时 / 取件超时）上迟到操作的错误分类
  稳定明确；12 格口并发混合终止下格口只释放一次、无悬挂占用与残留凭证索引；
- 交接记录不含任何凭证明文或包裹敏感原文。

测试通过可注入的 `fakeClock` 显式推进时间，无需真实睡眠，竞态测试可重复。
