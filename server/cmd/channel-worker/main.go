package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wecom"
	"github.com/costrict/costrict-web/server/internal/channel/adapters/wechat"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
)

func main() {
	logger.Init(logger.Config{
		Dir:          "./logs",
		FilePrefix:   "channel-worker",
		MaxAgeDays:   7,
		Console:      true,
		ConsoleLevel: "warn",
	})

	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	if err := db.AutoMigrate(&models.ChannelConfig{}); err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	channel.RegisterAdapter(wechat.NewWeChatAdapter())
	channel.RegisterAdapter(wecom.NewWeComAdapter(cfg.Channels.WeCom))

	worker := channel.NewChannelWorker(db, &channel.EchoMessageHandler{}, cfg.Channels.EnabledTypes)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Println("Channel worker starting...")
	if err := worker.Run(ctx); err != nil {
		log.Printf("Channel worker stopped: %v", err)
	}
}
