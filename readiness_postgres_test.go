package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/health"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// Against a real PostgreSQL: while the database takes no connections,
// `skymail-backend healthcheck` (GET /ready?gate=skip) fails and /ready
// answers 503; when it takes them again, both pass again without a restart.
func TestHealthcheckFollowsThePostgresDatabase(t *testing.T) {
	postgres := testpostgres.StartDatabase(t)
	database, err := openReadinessDatabase(postgres.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if got := database.Config().MaxConns; got != readinessDatabaseConns {
		t.Fatalf("readiness pool MaxConns=%d, want %d", got, readinessDatabaseConns)
	}
	// Every check pings.
	readiness := health.NewReadiness(database, health.Options{MaxAge: 1, Logf: t.Logf})
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	registerPublicRoutes(app, nil, readiness)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	defer func() { _ = app.Shutdown() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	check := func() (int, string) {
		var errOut bytes.Buffer
		code := runHealthcheck(nil, env(map[string]string{"APP_PORT": port}), &errOut)
		return code, errOut.String()
	}
	waitFor(t, "the first healthy check", func() bool { code, _ := check(); return code == 0 })

	admin, err := pgx.Connect(context.Background(), strings.Replace(postgres.URL, "/skymailtest?", "/postgres?", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	// The database goes away for SkyMail: no new connection, the open ones
	// ended.
	exec(`ALTER DATABASE skymailtest ALLOW_CONNECTIONS false`)
	exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'skymailtest'`)
	code, why := check()
	if code != 1 || !strings.Contains(why, "503") {
		t.Fatalf("database away: exit %d %q, want 1 and a 503", code, why)
	}

	exec(`ALTER DATABASE skymailtest ALLOW_CONNECTIONS true`)
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, why = check()
		if code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("database back: exit %d %q", code, why)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
