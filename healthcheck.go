package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/skymail-backend/internal/config"
)

// healthcheckCommandName is the container's health check (Dockerfile
// HEALTHCHECK, Swarm's Health Check): `skymail-backend healthcheck` asks this
// container's own SkyMail for GET /ready?gate=skip, that is whether it is not
// shutting down and its database answers, leaving out the account access
// gate's Redis (docs/health-and-shutdown.md). The image needs no curl or
// wget for it.
const healthcheckCommandName = "healthcheck"

// healthcheckTimeout stays under Swarm's timeout for the check (5 s) and
// over readiness's own database ping (health.DefaultPingTimeout, 2 s).
const healthcheckTimeout = 3 * time.Second

// healthcheckPath is readiness without the account access gate.
const healthcheckPath = "/ready?gate=skip"

// runHealthcheck exits 0 when SkyMail on APP_PORT (3000 unset) answers
// /ready?gate=skip with 2xx, and 1 otherwise; Docker reserves 2. It writes
// why to stderr, which Docker keeps with the check's result (docker
// inspect).
func runHealthcheck(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "usage: skymail-backend %s (no arguments; APP_PORT is read from the environment)\n", healthcheckCommandName)
		return 1
	}
	port := strings.TrimSpace(getenv("APP_PORT"))
	if port == "" || port == "0" {
		port = "3000"
	}
	client := &http.Client{Timeout: healthcheckTimeout}
	response, err := client.Get("http://127.0.0.1:" + port + healthcheckPath)
	if err != nil {
		fmt.Fprintf(stderr, "skymail-backend healthcheck: %v\n", err)
		return 1
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		fmt.Fprintf(stderr, "skymail-backend healthcheck: %s answered %d\n", healthcheckPath, response.StatusCode)
		return 1
	}
	return 0
}

// healthcheckEnv reads APP_PORT as the server does: the environment, over a
// .env in the working directory when there is one.
func healthcheckEnv() func(string) string {
	if err := config.LoadEnv(); err != nil {
		return os.Getenv
	}
	return config.Value
}

// readinessDatabaseConns are /ready's own connections to the database. It
// pings on one of its own so that a main pool whose connections are all busy
// (a slow moment, not an outage) does not make the task look broken and get
// it restarted under load. One more connection per task in the database's
// budget.
const readinessDatabaseConns = 1

// readinessConnLifetime is how long readiness keeps its connection.
const readinessConnLifetime = 24 * time.Hour

// openReadinessDatabase opens readiness's pool on databaseURL. Nothing
// connects until the first ping.
func openReadinessDatabase(databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx's error may quote the address, password and all.
		return nil, errors.New("readiness: the database address cannot be read")
	}
	config.MaxConns = readinessDatabaseConns
	config.MinConns = 0
	// The connection lives long: re-made every hour (pgx's default), it
	// could meet the database at max_connections and fail the health check
	// of a SkyMail that is fine.
	config.MaxConnLifetime = readinessConnLifetime
	config.MaxConnIdleTime = readinessConnLifetime
	return pgxpool.NewWithConfig(context.Background(), config)
}
