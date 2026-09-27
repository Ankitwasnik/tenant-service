package main

import (
	"testing"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr string
	}{
		{
			name: "all set",
			env: map[string]string{
				"DATABASE_URL": "postgres://db",
				"AMQP_URL":     "amqp://mq",
				"HTTP_ADDR":    ":9090",
			},
			want: config{DatabaseURL: "postgres://db", AMQPURL: "amqp://mq", HTTPAddr: ":9090"},
		},
		{
			name: "HTTP_ADDR defaults",
			env: map[string]string{
				"DATABASE_URL": "postgres://db",
				"AMQP_URL":     "amqp://mq",
			},
			want: config{DatabaseURL: "postgres://db", AMQPURL: "amqp://mq", HTTPAddr: ":8080"},
		},
		{
			name:    "required variables missing",
			env:     map[string]string{},
			wantErr: "missing required environment variables: DATABASE_URL, AMQP_URL",
		},
		{
			name:    "one required variable missing",
			env:     map[string]string{"DATABASE_URL": "postgres://db"},
			wantErr: "missing required environment variables: AMQP_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(func(key string) string { return tt.env[key] })

			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}
