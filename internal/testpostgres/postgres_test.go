package testpostgres

import (
	"context"
	"testing"
	"time"
)

func TestWaitForPublishedPortRetriesSuccessfulEmptyOutput(t *testing.T) {
	t.Parallel()

	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hostport, err := waitForPublishedPort(ctx, 0, func() ([]byte, error) {
		calls++
		if calls < 3 {
			return []byte("\n"), nil
		}
		return []byte("127.0.0.1:49152\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if hostport != "127.0.0.1:49152" || calls != 3 {
		t.Fatalf("hostport=%q calls=%d", hostport, calls)
	}
}
