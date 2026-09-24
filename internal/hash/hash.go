// Package hash 收口系统使用的摘要算法。
//
// 全系统只用一个算法（BLAKE3-256）：分片、校验分片、文件版本切片、
// 上传下载的内容校验、以及加密口令的校验值都用它。把算法收在这一个包里，
// 将来换算法或调整输出长度只需要改这里。
package hash

import (
	"bytes"

	"github.com/zeebo/blake3"
)

// Size 是摘要的字节长度，也是落库时 hash 列的固定长度。
const Size = 32

// Sum 计算 data 的摘要，返回可直接落库的切片。
func Sum(data []byte) []byte {
	sum := blake3.Sum256(data)
	return sum[:]
}

// SumArray 计算摘要并返回定长数组；需要塞进固定长度字段
// （例如 stripeResult.parityHash）时用它。
func SumArray(data []byte) [Size]byte {
	return blake3.Sum256(data)
}

// Equal 判断 data 的摘要是否与 want 一致。
//
// want 为空表示"没有可校验的期望值"，一律返回 true——历史数据可能没有
// 记录摘要，此时只做读取、不做校验。
func Equal(data, want []byte) bool {
	if len(want) == 0 {
		return true
	}
	sum := blake3.Sum256(data)
	return bytes.Equal(sum[:], want)
}
