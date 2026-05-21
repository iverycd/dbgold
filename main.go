package main

import (
	"dbgold/api"
	"dbgold/api/handler"
	"dbgold/config"
	"dbgold/middleware"
	"dbgold/store"
	"log"
)

func main() {
	cfg := config.Load()

	store.Init(cfg)
	if err := store.EnsureAdminExists(cfg.AdminUser, cfg.AdminPass); err != nil {
		log.Fatalf("failed to ensure admin: %v", err)
	}

	middleware.SetJWTSecret(cfg.JWTSecret)
	handler.SetJWTSecret(cfg.JWTSecret)

	r := api.NewRouter()
	log.Printf("Starting server on :%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
