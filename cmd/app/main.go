// Command app is the StreetFood customer API.
package main

import (
	"log"

	"github.com/Calteras/streetfood-customer-api/config"
	"github.com/Calteras/streetfood-customer-api/internal/app"
)

func main() {
	// Settings come from the environment, the stage's files, or .env for local runs.
	if err := config.LoadEnv(); err != nil {
		log.Fatalf("Config error: %s", err)
	}

	cfg, err := config.New()
	if err != nil {
		log.Fatalf("Config error: %s", err)
	}

	app.Run(cfg)
}
