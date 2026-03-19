package main

import (
	"fmt"
	"log"
	"os"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	sql := `
-- 删除旧的外键约束（如果存在）
ALTER TABLE capability_versions DROP CONSTRAINT IF EXISTS fk_capability_items_versions;

-- 添加新的级联删除外键约束
ALTER TABLE capability_versions 
ADD CONSTRAINT fk_capability_items_versions 
FOREIGN KEY (item_id) REFERENCES capability_items(id) ON DELETE CASCADE;
`

	result := db.Exec(sql)
	if result.Error != nil {
		log.Fatalf("Failed to execute migration: %v", result.Error)
	}

	fmt.Println("Migration completed successfully!")
	fmt.Printf("Rows affected: %d\n", result.RowsAffected)
	
	os.Exit(0)
}
