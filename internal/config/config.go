package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Address              string
	DatabaseURL          string
	DataDir              string
	APIKey               string
	TenantID             string
	MaxFileSize          int64
	RequestTimeout       time.Duration
	UploadTimeout        time.Duration
	MaxConcurrentUploads int
	MaxConcurrentMerges  int
}

func Load() (Config, error) {
	config := Config{
		Address:     env("ADDRESS", ":8092"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		DataDir:     env("DATA_DIR", "./data"),
		APIKey:      env("API_KEY", "local-upload-key"),
		TenantID:    env("TENANT_ID", "demo-tenant"),
	}
	if config.DatabaseURL == "" {
		return config, fmt.Errorf("DATABASE_URL is required")
	}
	var err error
	config.MaxFileSize, err = strconv.ParseInt(env("MAX_FILE_SIZE", "4294967296"), 10, 64)
	if err != nil || config.MaxFileSize <= 0 || config.MaxFileSize > 1<<40 {
		return config, fmt.Errorf("MAX_FILE_SIZE must be positive and at most 1 TiB")
	}
	config.RequestTimeout, err = duration("REQUEST_TIMEOUT", "20m")
	if err != nil {
		return config, err
	}
	config.UploadTimeout, err = duration("UPLOAD_TIMEOUT", "2m")
	if err != nil {
		return config, err
	}
	if len(config.APIKey) < 12 || config.TenantID == "" {
		return config, fmt.Errorf("API_KEY must have at least 12 characters and TENANT_ID is required")
	}
	if config.MaxConcurrentUploads, err = positiveInt("MAX_CONCURRENT_UPLOADS", "4", 128); err != nil {
		return config, err
	}
	if config.MaxConcurrentMerges, err = positiveInt("MAX_CONCURRENT_MERGES", "1", 10); err != nil {
		return config, err
	}
	return config, nil
}

func positiveInt(key, fallback string, maximum int) (int, error) {
	value, err := strconv.Atoi(env(key, fallback))
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("%s must be between 1 and %d", key, maximum)
	}
	return value, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key, fallback string) (time.Duration, error) {
	value, err := time.ParseDuration(env(key, fallback))
	if err != nil || value < time.Second || value > time.Hour {
		return 0, fmt.Errorf("%s must be between 1s and 1h", key)
	}
	return value, nil
}
