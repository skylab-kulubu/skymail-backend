// Package testpostgres provides a disposable PostgreSQL fixture for integration tests.
package testpostgres

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start launches PostgreSQL and registers all cleanup with t. Tests are skipped
// when Docker is unavailable.
func Start(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	name := fmt.Sprintf("skymail-postgres-test-%d", time.Now().UnixNano())
	out, err := exec.Command(
		"docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=postgres",
		"-e", "POSTGRES_DB=skymailtest",
		"-p", "127.0.0.1::5432",
		"postgres:17-alpine",
	).CombinedOutput()
	if err != nil {
		t.Skipf("docker run postgres: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	portOut, err := exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port: %v %s", err, portOut)
	}
	hostport := strings.Split(strings.TrimSpace(string(portOut)), "\n")[0]
	if i := strings.LastIndex(hostport, "://"); i >= 0 {
		hostport = hostport[i+3:]
	}

	databaseURL := fmt.Sprintf("postgres://postgres:postgres@%s/skymailtest?sslmode=disable", hostport)
	deadline := time.Now().Add(30 * time.Second)
	for {
		pool, poolErr := pgxpool.New(context.Background(), databaseURL)
		if poolErr == nil {
			poolErr = pool.Ping(context.Background())
		}
		if poolErr == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		if pool != nil {
			pool.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never became ready: %v", poolErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
