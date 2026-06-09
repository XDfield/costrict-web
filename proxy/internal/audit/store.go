package audit

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Store struct {
	db *gorm.DB
}

func NewStore(databaseURL string, maxOpen, maxIdle, maxLifetimeSec int) (*Store, error) {
	if err := ensureDatabase(databaseURL); err != nil {
		return nil, fmt.Errorf("ensure database: %w", err)
	}

	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql.DB: %w", err)
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Duration(maxLifetimeSec) * time.Second)

	return &Store{db: db}, nil
}

func ensureDatabase(databaseURL string) error {
	u := databaseURL
	var dbName string
	if idx := strings.LastIndex(u, "/"); idx != -1 {
		dbName = u[idx+1:]
	} else {
		return nil
	}
	if idx := strings.Index(dbName, "?"); idx != -1 {
		dbName = dbName[:idx]
	}
	if dbName == "" || dbName == "postgres" {
		return nil
	}

	maintenanceDSN := u[:strings.LastIndex(u, "/")+1] + "postgres"
	if idx := strings.Index(u, "?"); idx != -1 && idx > strings.LastIndex(u, "/") {
		maintenanceDSN = u[:strings.LastIndex(u, "/")+1] + "postgres" + u[idx:]
	}

	db, err := sql.Open("postgres", maintenanceDSN)
	if err != nil {
		return fmt.Errorf("open maintenance connection: %w", err)
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow("SELECT 1 FROM pg_database WHERE datname = $1", dbName).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("check database existence: %w", err)
	}
	if err == sql.ErrNoRows || !exists {
		if _, err := db.Exec("CREATE DATABASE " + quoteIdent(dbName)); err != nil {
			return fmt.Errorf("create database %s: %w", dbName, err)
		}
	}
	return nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (s *Store) Ping() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

func (s *Store) AutoMigrate() error {
	return s.db.AutoMigrate(&AuditLog{}, &AuditFile{}, &AuditTool{})
}

func (s *Store) InsertBatch(entries []*AuditLog) error {
	if len(entries) == 0 {
		return nil
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, entry := range entries {
			if err := tx.Create(entry).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) CleanBefore(before time.Time) error {
	var files []AuditFile
	var tools []AuditTool

	s.db.Where("created_at < ?", before).Find(&files)
	s.db.Where("created_at < ?", before).Find(&tools)

	auditIDs := make([]string, 0)
	s.db.Model(&AuditLog{}).Where("created_at < ?", before).Pluck("id", &auditIDs)

	if len(auditIDs) > 0 {
		s.db.Where("audit_id IN ?", auditIDs).Delete(&AuditFile{})
		s.db.Where("audit_id IN ?", auditIDs).Delete(&AuditTool{})
	}

	return s.db.Where("created_at < ?", before).Delete(&AuditLog{}).Error
}

func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
