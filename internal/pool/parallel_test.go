package pool

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelEach(t *testing.T) {
	const n = 20
	var ran int64
	errs := parallelEach(n, 4, func(i int) error {
		atomic.AddInt64(&ran, 1)
		if i%5 == 0 {
			return errIndex{index: i}
		}
		return nil
	})
	if len(errs) != n {
		t.Fatalf("返回 %d 个错误槽, want %d", len(errs), n)
	}
	if atomic.LoadInt64(&ran) != n {
		t.Fatalf("执行次数 = %d, want %d", ran, n)
	}
	for i, err := range errs {
		var wantErr error
		if i%5 == 0 {
			wantErr = errIndex{index: i}
		}
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("第 %d 个错误 = %v, want %v", i, err, wantErr)
		}
	}
}

func TestParallelEachRespectsLimit(t *testing.T) {
	const (
		n     = 16
		limit = 3
	)
	var running, peak int64
	errs := parallelEach(n, limit, func(int) error {
		cur := atomic.AddInt64(&running, 1)
		for {
			old := atomic.LoadInt64(&peak)
			if cur <= old || atomic.CompareAndSwapInt64(&peak, old, cur) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt64(&running, -1)
		return nil
	})
	if firstErr(errs) != nil {
		t.Fatalf("不该有错误: %v", firstErr(errs))
	}
	if got := atomic.LoadInt64(&peak); got > limit {
		t.Fatalf("并发峰值 = %d, 超过上限 %d", got, limit)
	}
}

func TestParallelEachEmpty(t *testing.T) {
	errs := parallelEach(0, 4, func(int) error {
		t.Fatal("空任务不该调用 fn")
		return nil
	})
	if len(errs) != 0 {
		t.Fatalf("返回 %d 个错误槽, want 0", len(errs))
	}
	if firstErr(errs) != nil {
		t.Fatal("空结果不该有错误")
	}
}

func TestFirstErr(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		errs []error
		want error
	}{
		{"全 nil", []error{nil, nil}, nil},
		{"空切片", nil, nil},
		{"第一个错误", []error{nil, boom, errors.New("later")}, boom},
	}
	for _, tt := range tests {
		if got := firstErr(tt.errs); got != tt.want {
			t.Errorf("%s: firstErr = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParallelEachIsRaceFree(t *testing.T) {
	// parallelEach 被 parity/rebuild/chunk 读写共用，这里顺带用 -race 兜一遍
	var wg sync.WaitGroup
	for round := 0; round < 4; round++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := make([]int, 64)
			errs := parallelEach(len(data), 0, func(i int) error {
				data[i] = i * i
				return nil
			})
			if firstErr(errs) != nil {
				t.Error(firstErr(errs))
			}
			for i, v := range data {
				if v != i*i {
					t.Errorf("data[%d] = %d, want %d", i, v, i*i)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// errIndex 带下标的一次性错误类型，方便断言"哪个槽出错"。
type errIndex struct{ index int }

func (e errIndex) Error() string { return "index error" }
