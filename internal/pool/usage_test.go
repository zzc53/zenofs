package pool

import (
	"testing"

	"github.com/zzc53/zenofs/internal/testutil"
)

func TestUsageCountsRealOccupancy(t *testing.T) {
	env, pm := newEnv(t)
	p := env.NewPool("p", 2, 1, 4096)

	chunks, err := pm.AddChunks(p.Id, [][]byte{testutil.RandBytes(7, 1024)})
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Flush(chunkIds(chunks)); err != nil {
		t.Fatal(err)
	}
	if !pm.calculateStripeParity() {
		t.Fatal("calculateStripeParity 没有算 parity")
	}

	got, err := pm.Usage(p.Id)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if got.DataBytes != 1024 {
		t.Errorf("DataBytes = %d, want 1024", got.DataBytes)
	}
	// parity 的长度取该条带最长 data chunk（1024），所以是 1×1024
	if got.ParityBytes != 1024 {
		t.Errorf("ParityBytes = %d, want 1024", got.ParityBytes)
	}
	// 槽位：1 个 data + 1 个 parity 已占用，还剩 1 个空 data 槽
	if got.UsedSlots != 2 {
		t.Errorf("UsedSlots = %d, want 2", got.UsedSlots)
	}
	if got.FreeSlots != 1 {
		t.Errorf("FreeSlots = %d, want 1", got.FreeSlots)
	}
}

func TestUsageUnknownPool(t *testing.T) {
	_, pm := newEnv(t)
	if _, err := pm.Usage(12345); err == nil {
		t.Fatal("不存在的池应当报错")
	}
}
