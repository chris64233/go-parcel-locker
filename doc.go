// Package goparcellocker 实现快递柜格口的预约、投递与一次性取件流程，
// 支持并发安全的格口分配、凭证摘要存储、超时推进与脱敏交接记录。
//
// 典型流程见 [Locker]：[Locker.Reserve] 按尺寸整笔申请格口并取得短期有效的
// 投递凭证；[Locker.Deliver] 消费投递凭证并生成一次性取件凭证；
// [Locker.Pickup] 完成取件并精确一次释放格口；[Locker.Cancel] 与
// [Locker.SweepExpired] 处理投递前取消与两类超时；[Locker.Status]、
// [Locker.Reservation] 与 [Locker.Handoffs] 提供状态查询与审计。
package goparcellocker
