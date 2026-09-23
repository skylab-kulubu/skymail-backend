package testpostgres_test

import (
	"net/url"
	"os/exec"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The fixture starts postgres, whose image declares VOLUME
// /var/lib/postgresql/data, so each container gets an anonymous volume. --rm
// discards that volume only when the container exits by itself; the fixture
// forces it out instead, which leaves the volume behind unless -v is given.
// One per test run adds up quietly until the disk is full.
func TestStartDatabaseLeavesNoVolumeBehind(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	var volumes []string

	// The fixture's cleanup runs when its own test ends, so the assertion has to
	// live outside it.
	t.Run("fixture", func(t *testing.T) {
		database := testpostgres.StartDatabase(t)
		volumes = containerVolumes(t, database.URL)
	})

	if len(volumes) == 0 {
		t.Skip("container reported no anonymous volume; nothing to leak")
	}

	for _, volume := range volumes {
		if err := exec.Command("docker", "volume", "inspect", volume).Run(); err == nil {
			// Do not leave the evidence behind either.
			_ = exec.Command("docker", "volume", "rm", volume).Run()
			t.Errorf("volume %s survived the fixture's cleanup", volume)
		}
	}
}

// containerVolumes returns the anonymous volumes of the one container this test
// started. It is found by the published port in its URL rather than by the name
// prefix: another test, another session or an orphan from an earlier run can
// have a fixture container alive at the same time, and inspecting those would
// fail this test for someone else's volume.
func containerVolumes(t *testing.T, databaseURL string) []string {
	t.Helper()

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	port := parsed.Port()
	if port == "" {
		t.Fatalf("database url %q has no port to identify the container by", databaseURL)
	}

	names, err := exec.Command("docker", "ps", "--filter", "name=skymail-postgres-test-", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("list fixture containers: %v", err)
	}

	for _, name := range strings.Fields(string(names)) {
		published, err := exec.Command("docker", "port", name, "5432/tcp").Output()
		if err != nil || !strings.Contains(string(published), ":"+port) {
			continue
		}

		out, err := exec.Command("docker", "inspect", name,
			"--format", `{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}`).Output()
		if err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		return strings.Fields(string(out))
	}

	t.Fatalf("no fixture container is published on port %s", port)
	return nil
}
