package main

import (
	"io"
	"testing"
	"time"

	"github.com/Ankitwasnik/tenant-service/internal/worker"
)

func TestParseConfig(t *testing.T) {
	base := map[string]string{"AMQP_URL": "amqp://mq"}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    worker.Config
		wantErr bool
	}{
		{
			name: "defaults", env: base,
			want: worker.Config{MinDelay: 500 * time.Millisecond, MaxDelay: 2 * time.Second, FailRate: 0},
		},
		{
			name: "flags", env: base,
			args: []string{"--min-delay-ms=10", "--max-delay-ms=20", "--fail-rate=1"},
			want: worker.Config{MinDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond, FailRate: 1},
		},
		{
			name: "env vars", env: with(map[string]string{"WORKER_MIN_DELAY_MS": "1", "WORKER_MAX_DELAY_MS": "2", "WORKER_FAIL_RATE": "0.5"}),
			want: worker.Config{MinDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, FailRate: 0.5},
		},
		{
			name: "flag overrides env", env: with(map[string]string{"WORKER_FAIL_RATE": "0.5"}),
			args: []string{"--fail-rate=1"},
			want: worker.Config{MinDelay: 500 * time.Millisecond, MaxDelay: 2 * time.Second, FailRate: 1},
		},
		{name: "min above max", env: base, args: []string{"--min-delay-ms=3000"}, wantErr: true},
		{name: "fail rate above 1", env: base, args: []string{"--fail-rate=1.5"}, wantErr: true},
		{name: "negative fail rate", env: base, args: []string{"--fail-rate=-0.1"}, wantErr: true},
		{name: "negative delay", env: base, args: []string{"--min-delay-ms=-1"}, wantErr: true},
		{name: "unknown flag", env: base, args: []string{"--seed=1"}, wantErr: true},
		{name: "stray argument", env: base, args: []string{"extra"}, wantErr: true},
		{name: "bad env value", env: with(map[string]string{"WORKER_FAIL_RATE": "lots"}), wantErr: true},
		{name: "missing AMQP_URL", env: map[string]string{}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, url, err := parseConfig(tt.args, func(k string) string { return tt.env[k] }, io.Discard)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseConfig succeeded with %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want || url != "amqp://mq" {
				t.Fatalf("parseConfig = %+v, %q; want %+v", got, url, tt.want)
			}
		})
	}
}
