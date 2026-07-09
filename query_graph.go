package main

import (
	"database/sql"
	"fmt"
	"log"

	"feishu-companion-bot/internal/config"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	cfg := config.Load()
	dsn := cfg.MemoryDatabaseDSN
	if dsn == "" {
		log.Fatal("MemoryDatabaseDSN is empty")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	// 1. Query all entities
	fmt.Println("=== 实体列表 (Entities) ===")
	rowsEnt, err := db.Query("SELECT id, name, category, COALESCE(description, '') FROM knowledge_entities")
	if err != nil {
		log.Fatalf("Query entities failed: %v", err)
	}
	defer rowsEnt.Close()

	hasEnt := false
	for rowsEnt.Next() {
		hasEnt = true
		var id, name, cat, desc string
		if err := rowsEnt.Scan(&id, &name, &cat, &desc); err == nil {
			fmt.Printf("- [%s] ID: %s, Category: %s, Description: %s\n", name, id, cat, desc)
		}
	}
	if !hasEnt {
		fmt.Println("(无实体记录)")
	}

	// 2. Query all relations
	fmt.Println("\n=== 关系三元组 (Relations) ===")
	rowsRel, err := db.Query(`
		SELECT e1.name, r.relation, e2.name, r.confidence
		FROM knowledge_relations r
		JOIN knowledge_entities e1 ON r.src_id = e1.id
		JOIN knowledge_entities e2 ON r.dst_id = e2.id
	`)
	if err != nil {
		log.Fatalf("Query relations failed: %v", err)
	}
	defer rowsRel.Close()

	hasRel := false
	for rowsRel.Next() {
		hasRel = true
		var src, rel, dst string
		var conf float64
		if err := rowsRel.Scan(&src, &rel, &dst, &conf); err == nil {
			fmt.Printf("- (%s) --[%s]--> (%s) [Confidence: %.2f]\n", src, rel, dst, conf)
		}
	}
	if !hasRel {
		fmt.Println("(无关系记录)")
	}

	fmt.Println("\n=== 底层记忆与归档大表数据量统计 (Big Tables Counts) ===")
	var memCount, chatCount, mediaCount int
	_ = db.QueryRow("SELECT COUNT(*) FROM bot_memories").Scan(&memCount)
	_ = db.QueryRow("SELECT COUNT(*) FROM chat_archive").Scan(&chatCount)
	_ = db.QueryRow("SELECT COUNT(*) FROM media_archive").Scan(&mediaCount)

	fmt.Printf("- 长期记忆表 (bot_memories) 行数: %d\n", memCount)
	fmt.Printf("- 微信聊天归档表 (chat_archive) 行数: %d\n", chatCount)
	fmt.Printf("- 微信多媒体归档表 (media_archive) 行数: %d\n", mediaCount)
}
