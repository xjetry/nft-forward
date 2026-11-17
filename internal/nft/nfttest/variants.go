// Package nfttest 提供围绕 nft.Rule 写测试时用的夹具。只给测试用。
package nfttest

import (
	"fmt"
	"reflect"

	"nft-forward/internal/nft"
)

// FieldVariants 对 base 的每个字段各造一条规则，它只在那一个字段上与 base 不同，
// 按字段名索引。字段从 nft.Rule 的类型定义里枚举，所以 Rule 加了字段这里自动多出
// 一条。
//
// 用它可以把“某个读者对每个字段的反应”与 Rule.Dataplane、Rule.WithoutPanelBinding
// 的归类逐字段对照，不必在每个读者的测试里各抄一份字段清单。
//
// 取值规则：Mode 换成另一个生效的模式、Proto 换成另一个协议、DestIP 换成另一个
// 地址（三者都保持取值合法）；其余字符串在末尾加一个字符，整数加一。遇到别的字段
// 类型直接 panic——那时要在这里补上取值规则。
func FieldVariants(base nft.Rule) map[string]nft.Rule {
	out := map[string]nft.Rule{}
	t := reflect.TypeOf(base)
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		variant := base
		f := reflect.ValueOf(&variant).Elem().Field(i)
		switch {
		case name == "Mode":
			if base.EffectiveMode() == nft.ModeUserspace {
				f.SetString(nft.ModeKernel)
			} else {
				f.SetString(nft.ModeUserspace)
			}
		case name == "Proto":
			if base.Proto == "udp" {
				f.SetString("tcp")
			} else {
				f.SetString("udp")
			}
		case name == "DestIP":
			if base.DestIP == "192.0.2.77" {
				f.SetString("192.0.2.78")
			} else {
				f.SetString("192.0.2.77")
			}
		case f.Kind() == reflect.String:
			f.SetString(f.String() + "x")
		case f.CanInt():
			f.SetInt(f.Int() + 1)
		default:
			panic(fmt.Sprintf("nfttest: nft.Rule.%s 的类型 %s 没有取值规则", name, f.Kind()))
		}
		out[name] = variant
	}
	return out
}
