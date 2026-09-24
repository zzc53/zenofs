package hash

import "testing"

func TestSumIsDeterministicAndFixedSize(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"short", []byte("zenofs")},
		{"binary", []byte{0x00, 0xff, 0x00, 0x7f}},
	}
	for _, tt := range tests {
		got := Sum(tt.data)
		if len(got) != Size {
			t.Fatalf("%s: len = %d, want %d", tt.name, len(got), Size)
		}
		if again := Sum(tt.data); string(again) != string(got) {
			t.Fatalf("%s: 同一输入两次摘要不一致", tt.name)
		}
	}

	// 差一个字节就必须换一个摘要（校验分片/切片都靠这个前提）
	if string(Sum([]byte("zenofs"))) == string(Sum([]byte("zenofs!"))) {
		t.Fatal("不同输入的摘要相同")
	}
}

func TestSumArrayMatchesSum(t *testing.T) {
	data := []byte("stripe parity payload")
	arr := SumArray(data)
	slice := Sum(data)
	if len(slice) != Size {
		t.Fatalf("Sum len = %d, want %d", len(slice), Size)
	}
	for i := range arr {
		if arr[i] != slice[i] {
			t.Fatalf("SumArray 与 Sum 不一致（位置 %d）", i)
		}
	}
}

func TestEqual(t *testing.T) {
	data := []byte("payload")
	sum := Sum(data)

	tests := []struct {
		name string
		data []byte
		want []byte
		ok   bool
	}{
		{"匹配", data, sum, true},
		{"数据被改动", []byte("payloaD"), sum, false},
		{"期望值被改动", data, Sum([]byte("other")), false},
		{"期望值长度不对", data, sum[:16], false},
		{"没有期望值时不校验（历史数据）", data, nil, true},
		{"没有期望值时空 want 也算通过", data, []byte{}, true},
	}
	for _, tt := range tests {
		if got := Equal(tt.data, tt.want); got != tt.ok {
			t.Errorf("%s: Equal = %v, want %v", tt.name, got, tt.ok)
		}
	}
}
