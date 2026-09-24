package vfs

import (
	"bytes"
	"testing"

	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/testutil"
)

// 写入池的字节是"压缩 + 加密"之后的结果，它必须仍然不超过池的 chunk 上限——
// 否则文件大小正好是切片整数倍时，满切片会以 CHUNK_SIZE_EXCEED 被拒收。
// 这里用不可压缩的随机数据（zstd 对它会略微膨胀）把边界钉住。
func TestSliceFitsPoolChunkAfterEncoding(t *testing.T) {
	cases := []struct {
		name      string
		comp, enc int8
	}{
		{"plain", CompressionNone, EncryptionNone},
		{"zstd", CompressionZstd, EncryptionNone},
		{"aesgcm", CompressionNone, EncryptionAESGCM},
		{"zstd+aesgcm", CompressionZstd, EncryptionAESGCM},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, pm := newEnv(t)
			p := env.NewPool("p", 2, 1, 256) // 池的 chunk 上限 256KB
			share := env.NewShare(p.Id, testutil.ShareOpts{
				Name: "s", UserID: 1, Permission: db.ShareWrite,
				Compression: tc.comp, Encryption: tc.enc,
			})
			fs := NewShareFS(pm, share, 1, db.ShareWrite)
			if tc.enc != EncryptionNone {
				if err := fs.SetPassword("pw-123456"); err != nil {
					t.Fatalf("SetPassword: %v", err)
				}
			}

			// 切片大小 + 编码开销必须落在池的上限之内
			sliceSize := fs.sliceSize()
			overhead := sliceOverheadFor(sliceSize, tc.comp, tc.enc)
			if sliceSize+overhead > p.ChunkSize*1024 {
				t.Fatalf("切片 %d + 开销 %d 超过池上限 %d", sliceSize, overhead, p.ChunkSize*1024)
			}

			// 恰好写满一个切片：随机数据不可压缩，zstd 会膨胀
			payload := testutil.RandBytes(7, int(sliceSize))
			writeFile(t, fs, "/full.bin", payload)
			got := readFile(t, fs, "/full.bin", sliceSize)
			if !bytes.Equal(got, payload) {
				t.Fatalf("内容不一致（拿到 %d 字节）", len(got))
			}

			// 再多一点就会跨片，同样要能写能读
			spill := append(payload, []byte("tail")...)
			writeFile(t, fs, "/spill.bin", spill)
			if got := readFile(t, fs, "/spill.bin", int64(len(spill))); !bytes.Equal(got, spill) {
				t.Fatalf("跨切片写入读回不一致")
			}
		})
	}
}

// 切片大小必须真的取自所属池：换个池，切片跟着变。
func TestSliceSizeFollowsPool(t *testing.T) {
	env, pm := newEnv(t)
	small := env.NewPool("small", 1, 0, 64)
	big := env.NewPool("big", 1, 0, 1024)

	for _, tc := range []struct {
		pool    db.Pool
		chunkKb int64
	}{
		{small, 64},
		{big, 1024},
	} {
		share := env.NewShare(tc.pool.Id, testutil.ShareOpts{
			Name: "s" + tc.pool.Name, UserID: 1, Permission: db.ShareWrite,
		})
		fs := NewShareFS(pm, share, 1, db.ShareWrite)
		want := tc.chunkKb * 1024
		if got := fs.sliceSize(); got != want {
			t.Fatalf("池 %dKB 的切片 = %d，期望 %d", tc.chunkKb, got, want)
		}
	}
}
