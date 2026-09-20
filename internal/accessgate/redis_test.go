package accessgate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type stubRedisReader struct {
	mgetValues []interface{}
	mgetErr    error
	getValue   string
	getErr     error
	keys       []string
	deadline   time.Time
}

func (s *stubRedisReader) MGet(ctx context.Context, keys ...string) *redis.SliceCmd {
	s.keys = append([]string(nil), keys...)
	s.deadline, _ = ctx.Deadline()
	return redis.NewSliceResult(s.mgetValues, s.mgetErr)
}

func (s *stubRedisReader) Get(ctx context.Context, key string) *redis.StringCmd {
	s.keys = []string{key}
	s.deadline, _ = ctx.Deadline()
	return redis.NewStringResult(s.getValue, s.getErr)
}

func TestRedisGateChecksHumanAndClientSubjectsWithSameContract(t *testing.T) {
	t.Parallel()

	for _, subject := range []string{
		"3babe8e3-4d2b-4d10-ba5f-95bfaf35c0dc",
		"service-account-skymail-backend",
	} {
		t.Run(subject, func(t *testing.T) {
			client := &stubRedisReader{mgetValues: []interface{}{ContractValue, nil}}
			gate := NewRedisGate(client, 50*time.Millisecond)
			if decision := gate.Check(context.Background(), subject); decision != Allowed {
				t.Fatalf("decision = %q, want allowed", decision)
			}
			wantKeys := []string{ContractKey, MarkerKey(subject)}
			if !reflect.DeepEqual(client.keys, wantKeys) {
				t.Fatalf("MGET keys = %v, want %v", client.keys, wantKeys)
			}
			if client.deadline.IsZero() {
				t.Fatal("MGET did not receive a bounded context")
			}
		})
	}
}

func TestRedisGateDecisionsAreFailClosed(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		values   []interface{}
		err      error
		decision Decision
	}{
		{name: "allowed only with exact contract and absent marker", values: []interface{}{ContractValue, nil}, decision: Allowed},
		{name: "blocked with exact marker", values: []interface{}{ContractValue, MarkerValue}, decision: Blocked},
		{name: "missing contract", values: []interface{}{nil, nil}, decision: Unavailable},
		{name: "wrong contract", values: []interface{}{"v2", nil}, decision: Unavailable},
		{name: "malformed marker", values: []interface{}{ContractValue, "blocked"}, decision: Unavailable},
		{name: "wrong response length", values: []interface{}{ContractValue}, decision: Unavailable},
		{name: "command failure", err: errors.New("connection refused"), decision: Unavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := NewRedisGate(&stubRedisReader{mgetValues: test.values, mgetErr: test.err}, time.Millisecond)
			if got := gate.Check(context.Background(), "subject"); got != test.decision {
				t.Fatalf("decision = %q, want %q", got, test.decision)
			}
		})
	}
}

func TestRedisGateReadinessRequiresExactContract(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		value string
		err   error
		ready bool
	}{
		{name: "exact contract", value: ContractValue, ready: true},
		{name: "wrong contract", value: "v2"},
		{name: "missing contract", err: redis.Nil},
		{name: "outage", err: errors.New("connection refused")},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &stubRedisReader{getValue: test.value, getErr: test.err}
			gate := NewRedisGate(client, time.Millisecond)
			err := gate.Ready(context.Background())
			if (err == nil) != test.ready {
				t.Fatalf("Ready() error = %v, want ready=%v", err, test.ready)
			}
			if !reflect.DeepEqual(client.keys, []string{ContractKey}) || client.deadline.IsZero() {
				t.Fatalf("GET keys=%v deadline=%v", client.keys, client.deadline)
			}
		})
	}
}
