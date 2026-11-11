package orchestrator

import "time"

// SetDeliveryBackoffForTest replaces the wait between delivery attempts so
// specs can exercise retries without waiting out the real schedule. It
// returns a function that restores the previous schedule.
func SetDeliveryBackoffForTest(wait time.Duration) func() {
	prev := deliveryBackoff
	deliveryBackoff = func(int) time.Duration { return wait }
	return func() { deliveryBackoff = prev }
}
