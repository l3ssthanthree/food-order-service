package domain

import "testing"

func TestCanTransition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from OrderStatus
		to   OrderStatus
		want bool
	}{
		{"pending accepted", OrderPendingRestaurant, OrderAccepted, true},
		{"pending rejected", OrderPendingRestaurant, OrderRejected, true},
		{"pending cancelled", OrderPendingRestaurant, OrderCancelled, true},
		{"accepted preparing", OrderAccepted, OrderPreparing, true},
		{"preparing ready", OrderPreparing, OrderReady, true},
		{"ready completed", OrderReady, OrderCompleted, true},
		{"cannot skip preparing", OrderAccepted, OrderReady, false},
		{"terminal completed", OrderCompleted, OrderPreparing, false},
		{"restaurant cannot cancel", OrderAccepted, OrderCancelled, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransition(test.from, test.to); got != test.want {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", test.from, test.to, got, test.want)
			}
		})
	}
}
