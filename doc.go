// Package distledger 是一个 Go 分销账务内核。
//
// 它只解决三件事：关系链、佣金账、资金流水。
// 规则（几级、多少比例、要不要门槛）全部外置为可插拔策略，
// 因为「规则」是无限长尾，而「账务」对所有分销场景都是常量：
//
//	订单归属某分销员 → 按规则算出若干级佣金 → 进冻结 → 到期可提现 → 退款扣回 → 出金
//
// # 设计铁律
//
//   - 切分线：能改变「钱流向」的决策进内核；只改变「钱数值」的决策做规则。
//     规则层永远不许修改状态。
//   - 默认不发钱：默认费率 0、默认 2 级，宁可配置麻烦，不可默认错发。
//   - 整数金额：一律 int64 最小货币单位（Money）+ 万分比整数费率（Rate），
//     全程禁止浮点参与运算。
//   - 账本不可变：佣金财务字段与资金流水只追加；冲正靠新增负记录。
//   - 零魔法：无 codegen、无反射注册、无全局单例。
//
// # 快速开始
//
//	led, err := distledger.New(distledger.Config{
//		Store: memory.New(),
//		Rules: distledger.Rules{Levels: 2, RateBP: []int32{500, 200}, FreezeDays: 7},
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	_, err = led.OnOrderPaid(ctx, distledger.OrderPaidEvent{
//		OrderID: "ORD-1", BuyerUserID: 1003, PaidAmount: distledger.Money(19900),
//	})
//
// 完整可运行示例见 examples/01-quickstart。
//
// # 它不做什么
//
// 推广链接、海报、排行榜、收益中心、分销员审核流程、支付与分账、
// 团队计酬/区域分红/链动、按拉人头或注册人数计酬、HTTP 路由与 Admin 后台。
// 完整清单见 PRD 第 3.2 节，那份清单是需求边界的法律。
package distledger
