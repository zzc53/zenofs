// Command zenofs 是 Zenofs RAID-like 系统的入口。
package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"

	"github.com/zzc53/zenofs/internal/config"
	"github.com/zzc53/zenofs/internal/db"
	"github.com/zzc53/zenofs/internal/pool"
)

func main() {
	// 1. 加载配置
	cfg := config.LoadConfig(os.Args)
	log.Printf("config loaded: %+v", cfg)

	// 2. 初始化数据库
	dbManager, err := db.New(cfg.DatabaseUrl)
	if err != nil {
		log.Fatalf("failed to init database: %+v", err)
	}
	if err := dbManager.AutoMigrate(); err != nil {
		log.Fatalf("failed to migrate database: %+v", err)
	}
	defer dbManager.Close()
	log.Printf("database connected [%s]", cfg.DatabaseUrl)

	ch := make(chan db.WriteQueue, 1000)
	poolManager := pool.New(dbManager, ch)
	if err := dbManager.AutoMigrate(); err != nil {
		log.Fatalf("failed to migrate database: %+v", err)
	}
	newPool, err := poolManager.AddPool("test1", 4096)
	if err != nil {
		log.Fatal(err)
	}
	dataDisks := 3
	parityDisks := 1
	for i := 1; i <= dataDisks; i++ {
		if _, err := poolManager.AddDisk(newPool.Id, fmt.Sprintf("/Users/zzc/code/disk/%d", i), int8(db.DataChunk), int8(db.LocalBackend), false); err != nil {
			log.Fatal(err)
		}
	}
	for j := dataDisks + 1; j <= dataDisks+parityDisks; j++ {
		if _, err := poolManager.AddDisk(newPool.Id, fmt.Sprintf("/Users/zzc/code/disk/%d", j), int8(db.DataChunk), int8(db.LocalBackend), true); err != nil {
			log.Fatal(err)
		}
	}
	newPool.Status = 0
	dbManager.DB.Save(&newPool)

	bytes := make([]byte, 4096)
	_, err = rand.Read(bytes)

	if err != nil {
		log.Fatal(err)
	}
	chunk, err := poolManager.AddChunk(newPool.Id, bytes)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%+v", chunk)

}
