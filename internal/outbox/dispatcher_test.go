package outbox

import (
	"testing"
	"time"
)

func TestRetryDelayIsExponentiallyBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{6, 32 * time.Second},
		{20, 32 * time.Second},
	}
	for _, test := range tests {
		if got := retryDelay(test.attempt); got != test.want {
			t.Fatalf("retryDelay(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}
