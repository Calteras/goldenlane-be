package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// LoadEnv fills unset environment variables from, in order:
//
//   - the dotenv file named by ENV_FILE (a deployed stage), or `.env` in development;
//   - for every NAME_FILE variable, the contents of that file as NAME (Docker and
//     Kubernetes secrets). Setting both NAME and NAME_FILE is an error.
//
// Variables that are already set always win.
func LoadEnv() error {
	switch path := os.Getenv("ENV_FILE"); {
	case path != "":
		if err := apply(path); err != nil {
			return err
		}
	case os.Getenv("APP_ENV") == "" || os.Getenv("APP_ENV") == EnvDevelopment:
		if _, err := os.Stat(".env"); err == nil {
			if err := apply(".env"); err != nil {
				return err
			}
		}
	}

	return applyFileVars()
}

func apply(path string) error {
	values, err := godotenv.Read(path)
	if err != nil {
		// The parser's own error can quote a line of the file, which may be a secret.
		return fmt.Errorf("cannot read environment file %s", path)
	}

	for k, v := range values {
		if _, set := os.LookupEnv(k); !set && v != "" {
			if err := os.Setenv(k, v); err != nil {
				return fmt.Errorf("set %s: %w", k, err)
			}
		}
	}

	return nil
}

var errBothSet = errors.New("set only one of NAME or NAME_FILE")

func applyFileVars() error {
	for _, kv := range os.Environ() {
		name, path, _ := strings.Cut(kv, "=")
		if !strings.HasSuffix(name, "_FILE") || name == "ENV_FILE" || path == "" {
			continue
		}

		target := strings.TrimSuffix(name, "_FILE")
		if os.Getenv(target) != "" {
			return fmt.Errorf("%w: %s", errBothSet, target)
		}

		data, err := os.ReadFile(path) //nolint:gosec // operator-supplied secret path
		if err != nil {
			return fmt.Errorf("cannot read %s", name)
		}

		if err := os.Setenv(target, strings.TrimRight(string(data), "\r\n")); err != nil {
			return fmt.Errorf("set %s: %w", target, err)
		}
	}

	return nil
}
