package config

import (
	"fmt"
	"os"
	"time"
)

type Kitchen struct {
	HTTPAddr           string
	DatabaseURL        string
	CallbackToken      string
	ShutdownTimeout    time.Duration
	OutboxPollInterval time.Duration
}

type DemoRestaurant struct {
	HTTPAddr       string
	KitchenBaseURL string
	PartnerToken   string
	CallbackToken  string
}

func LoadKitchen() (Kitchen, error) {
	cfg := Kitchen{
		HTTPAddr:           envOr("KITCHEN_HTTP_ADDR", ":8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		CallbackToken:      envOr("KITCHEN_CALLBACK_TOKEN", "kitchen-callback-secret"),
		ShutdownTimeout:    10 * time.Second,
		OutboxPollInterval: 500 * time.Millisecond,
	}
	if cfg.DatabaseURL == "" {
		return Kitchen{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func LoadDemoRestaurant() DemoRestaurant {
	return DemoRestaurant{
		HTTPAddr:       envOr("DEMO_HTTP_ADDR", ":8081"),
		KitchenBaseURL: envOr("KITCHEN_BASE_URL", "http://localhost:8080"),
		PartnerToken:   envOr("DEMO_RESTAURANT_TOKEN", "demo-restaurant-secret"),
		CallbackToken:  envOr("KITCHEN_CALLBACK_TOKEN", "kitchen-callback-secret"),
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
