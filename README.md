# go-parcel-locker

快递柜格口的**预约、投递与一次性取件**领域库（无外部依赖的内存实现）。
覆盖格口配置、整笔预约、凭证投递、取件码取件、预约取消、超时释放、状态查询
与脱敏交接记录，全部接口可被多个 goroutine 并发调用。

## 安装与测试

```bash
go test -race ./...      # 含并发竞态用例
go test -cover ./...     # 语句覆盖率（当前约 93%）
go vet ./...
```

## 快速上手

```go
locker := goparcellocker.NewLocker() // 默认 time.Now、投递 15min、取件 48h

locker.ConfigureCompartments([]goparcellocker.CompartmentSpec{
    {ID: "S1", Size: goparcellocker.SizeSmall},
    {ID: "M1", Size: goparcellocker.SizeMedium},
})

// 1) 快递员按包裹尺寸申请整批格口
receipt, _ := locker.Reserve("courier-7", []goparcellocker.Package{
    {TrackingNo: "SF123", Size: goparcellocker.SizeSmall, ContentHash: contentDigest},
}, time.Time{}) // 零值 deadline 表示使用默认投递有效期

// 2) 凭一次性投递凭证投递，拿到取件码
delivered, _ := locker.Deliver(receipt.Items[0].DeliveryToken, thePackage)

// 3) 收件人凭取件码取件，格口立即释放
locker.Pickup(delivered.PickupCode)
```

完整可执行示例见 `example_test.go`。

## 生命周期与状态机

每个包裹（预约条目）依次经过：

```
 reserved ──投递成功──▶ delivered ──取件成功──▶ picked_up     （终态）
    │                     │
    ├──取消──────────────▶ canceled          （终态，释放格口）
    ├──超过投递截止──────▶ expired(delivery) （终态，释放格口）
                          └──超过取件截止──▶ expired(pickup) （终态，释放格口）
```

- 投递截止：`Reserve` 时给定，或默认 15 分钟（`WithDeliveryTTL`）。
- 取件截止：投递成功时刻起默认 48 小时（`WithPickupTTL`）。
- `Advance(now)` 显式推进逻辑时钟并终结到期条目；所有写操作内部也会自动
  推进，因此定时任务只需周期调用 `Advance(time.Now())`。

## API 概览

| 方法 | 说明 |
| --- | --- |
| `NewLocker(opts...)` | 创建实例；`WithClock / WithDeliveryTTL / WithPickupTTL / WithHMACKey` |
| `ConfigureCompartments(specs)` | 整体设置格口；仍有未终结条目时返回 `ErrCompartmentsInUse` |
| `Reserve(courierID, pkgs, deadline)` | 整笔原子预约，返回每包一个**一次性投递凭证** |
| `Deliver(token, pkg)` | 投递；成功返回取件码，重复同内容调用幂等 |
| `Pickup(code)` | 一次性取件；成功即释放格口 |
| `CancelReservation(id)` | 整笔取消（仅所有包裹仍待投递时允许） |
| `Advance(now)` | 超时推进，返回本次终结的条目（投递/取件两阶段） |
| `Compartments / Reservation / Status` | 状态查询快照 |
| `HandoffRecords()` | 脱敏交接记录 |

错误哨兵（用 `errors.Is` 判定）：`ErrNoAvailableCompartment`、
`ErrInvalidDeadline`、`ErrCredentialInvalid`、`ErrPackageConflict`、
`ErrCredentialReplayed`、`ErrReservationCanceled`、`ErrReservationExpired`、
`ErrPackageExpired`、`ErrReservationNotFound`、`ErrReservationNotActive`、
`ErrCompartmentsInUse`、`ErrInvalidRequest`。

## 关键设计与保证

### 1. 整笔预约：要么全成，要么不占格口

- 分配在单把互斥锁内完成，先产出**完整分配方案再提交**；任一包裹找不到
  “仍可用且尺寸合适”的格口时，回滚临时占位，整笔返回
  `ErrNoAvailableCompartment`，**不会提前占用任何格口**。
- 分配策略为**最佳适配**（能容纳的最小尺寸，同尺寸取 ID 最小者），并按
  包裹尺寸从大到小落位，避免大件被小格口耗尽后无处可去。
- 并发预约串行化提交，同一格口不可能同时分给两个包裹。

### 2. 凭证只存摘要、只能消费一次

- 投递凭证（128 bit 随机熵，base32）明文只在预约回执中出现**一次**；
  服务端只保存以服务端密钥为键的 **HMAC-SHA256 摘要**，不存明文。
- 投递成功即推进状态机，投递凭证不可再次“消费”（重复提交走幂等/冲突分支，
  不会产生第二个取件码或第二次状态变更）。
- 取件码由投递凭证经 HMAC 确定性派生（120 bit 熵）：
  重复投递同一内容时无需保存明文即可稳定返回**同一个取件码**；
  取件成功后取件凭证立即删除，复用必失败。
- 摘要比较使用常量时间比较，避免计时侧信道。

### 3. 投递的幂等与冲突

- 相同投递凭证 + 相同包裹内容（尺寸 + `ContentHash`）重复提交：幂等返回
  首次结果，回执标记 `Replayed: true`，取件码与首次完全一致。
- 相同凭证但包裹信息变化：
  - 首次投递即与预约登记不符 → `ErrPackageConflict`，且**不消耗凭证**；
  - 已成功投递后再提交不同内容 → `ErrCredentialReplayed`，明确报冲突。

### 4. 取件 / 取消 / 超时的竞态安全

- 所有状态迁移都在同一把锁内按“检查当前状态 → 迁移”完成，终态不可逆，
  因此格口在取件、取消、过期三条路径中**至多释放一次**。
- 每个条目占用格口时带单调递增的 **generation（代际）**，释放前校验
  格口当前占用者确为本代际。于是：
  - 已取消 / 已过投递截止的预约不能再投递（旧凭证保留为“墓碑”，
    返回 `ErrReservationCanceled` / `ErrReservationExpired` 而非笼统失败）；
  - 旧取件码在包裹过期、格口被新包裹复用后再来，只定位到旧代际，
    **不可能影响后来使用该格口的包裹**。
- 取消与投递并发到达同一预约时恰有一方获胜：投递赢则取消随后失败，
  取消赢则投递得到 `ErrReservationCanceled`（测试以 200 轮竞态覆盖）。

### 5. 交接记录不泄漏凭证

`HandoffRecords()` 记录 `reserved / delivered / picked_up / canceled /
expired` 五类事件，字段只包含时间、预约/条目/格口/运单、快递员，以及凭证的
**HMAC 摘要与 12 位十六进制短标签**（仅供人工对账），任何路径都不会写出
投递凭证或取件码明文。

## 目录结构

```
doc.go           包占位
errors.go        业务错误哨兵
types.go         尺寸、包裹、回执、状态视图、交接记录等类型
credential.go    随机凭证、HMAC 摘要、取件码确定性派生
locker.go        Locker：配置/预约/投递/取件/取消/推进/查询
*_test.go        单元、竞态（-race）与可执行 Example
```

## 实现取舍

- 内存模型 + 全局互斥锁：对单机快递柜场景足够，且让状态机推理与竞态
  保证直接、可审计；持久化或分布式部署可在该领域模型外加存储层，
  代际字段对应乐观锁/ fencing token。
- 包裹“内容”的身份由调用方提供的 `ContentHash` 表达（建议对实际包裹
  内容做 SHA-256），库不接触明文内容。
