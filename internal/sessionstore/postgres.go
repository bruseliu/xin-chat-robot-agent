package sessionstore

import (
	"fmt"

	adksession "google.golang.org/adk/v2/session"
	adkdatabase "google.golang.org/adk/v2/session/database"
	"gorm.io/driver/postgres"
)

type PostgresConfig struct {
	DSN         string
	AutoMigrate bool
}

func NewPostgresService(cfg PostgresConfig) (adksession.Service, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("sessionstore: postgres DSN is required")
	}
	svc, err := adkdatabase.NewSessionService(postgres.Open(cfg.DSN))
	if err != nil {
		return nil, err
	}
	if cfg.AutoMigrate {
		if err := adkdatabase.AutoMigrate(svc); err != nil {
			return nil, err
		}
	}
	return svc, nil
}
