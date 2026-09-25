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

// Database is a disposable PostgreSQL fixture and its connection URL.
type Database struct {
	Pool *pgxpool.Pool
	URL  string
	// Container is the Docker container the server runs in, for the tools
	// that ship inside it (pg_dump, pg_restore) through docker exec. The
	// database is skymailtest and its superuser postgres.
	Container string
}

// Start launches PostgreSQL and registers all cleanup with t. Tests are skipped
// when Docker is unavailable.
func Start(t testing.TB) *pgxpool.Pool {
	t.Helper()
	return StartDatabase(t).Pool
}

// StartDatabase launches PostgreSQL and returns both its pool and URL.
func StartDatabase(t testing.TB) Database {
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
	// -v matters: --rm only discards the anonymous volume when the container
	// exits on its own. Forcing it out from the outside leaves the volume behind
	// for postgres's VOLUME /var/lib/postgresql/data, so every test run used to
	// leak one and they accumulated until the disk filled.
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })

	portCtx, cancelPort := context.WithTimeout(context.Background(), 30*time.Second)
	hostport, err := waitForPublishedPort(portCtx, 100*time.Millisecond, func() ([]byte, error) {
		return exec.Command("docker", "port", name, "5432/tcp").CombinedOutput()
	})
	cancelPort()
	if err != nil {
		t.Fatal(err)
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
			return Database{Pool: pool, URL: databaseURL, Container: name}
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

// waitForPublishedPort polls lookup until Docker reports the published host
// port. Docker returns success with empty output while the mapping is still
// pending, so a single call can yield an empty host:port and a connection URL
// that silently falls back to a unix socket.
func waitForPublishedPort(ctx context.Context, retryInterval time.Duration, lookup func() ([]byte, error)) (string, error) {
	var lastErr error
	for {
		out, err := lookup()
		if err == nil {
			if hostport := parsePublishedPort(out); hostport != "" {
				return hostport, nil
			}
			lastErr = fmt.Errorf("docker port returned empty output")
		} else {
			detail := strings.TrimSpace(string(out))
			if detail == "" {
				lastErr = fmt.Errorf("docker port: %w", err)
			} else {
				lastErr = fmt.Errorf("docker port: %w: %s", err, detail)
			}
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", fmt.Errorf("docker port was not published before timeout: %v: %w", lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func parsePublishedPort(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		hostport := strings.TrimSpace(line)
		if hostport == "" {
			continue
		}
		if i := strings.LastIndex(hostport, "://"); i >= 0 {
			hostport = hostport[i+3:]
		}
		if hostport != "" {
			return hostport
		}
	}
	return ""
}
