package pool

import "sync"

// ---------------------------------------------------------------
// 并发执行工具
// ---------------------------------------------------------------

// parallelEach 对下标 [0, n) 并发执行 fn。
//
// limit > 0 时限制同时在跑的任务数，limit <= 0 表示不限制。
// fn 只应写自己下标对应的槽位，不要跨下标共享可变状态。
//
// 与 errgroup 的 fail-fast 不同，这里保证所有任务都执行完——调用方常常需要
// 知道每个下标的成败（例如逐 chunk 记录写盘结果），因此某个任务报错不会
// 中断其它任务。返回值与 n 等长，第 i 个元素即 fn(i) 的返回值（nil 表示成功），
// 需要"其中一个错误"时用 firstErr 取。
func parallelEach(n, limit int, fn func(i int) error) []error {
	errsByIdx := make([]error, n)
	if n == 0 {
		return errsByIdx
	}

	// 不限并发或上限超过任务数时，容量取 n：名额永不枯竭，不产生阻塞。
	if limit <= 0 || limit > n {
		limit = n
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		sem <- struct{}{} // 名额用尽时在此阻塞，形成背压
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			errsByIdx[i] = fn(i)
		}()
	}
	wg.Wait()
	return errsByIdx
}

// firstErr 返回 errs 中第一个非 nil 的错误，全部成功时返回 nil。
func firstErr(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
