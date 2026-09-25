package service

import (
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/metrics"
)

// StartMetricsGaugeSync keeps the Prometheus gauges whose truth is a database
// row in step with that row: channel_up for every managed channel and
// byok_keys_active for the customer keys that are currently routable.
//
// Both could be updated only from the code paths that change them, but that
// leaves the gauges empty until the first relay request and wrong after a
// restart, an admin console edit, or a change made on another node. A periodic
// snapshot of the stored state is what makes them trustworthy, and it is why
// the first sync runs immediately rather than after one interval.
//
// Both reads are narrow, read-only projections; neither is on the relay path.
func StartMetricsGaugeSync(intervalSeconds int) {
	if !metrics.Enabled() {
		return
	}
	// A zero or negative SYNC_FREQUENCY must not turn this into a hot loop
	// against the channel table.
	interval := time.Duration(max(intervalSeconds, 10)) * time.Second
	for {
		if channels, err := model.GetChannelHealth(); err != nil {
			common.SysError("metrics: failed to read channel health: " + err.Error())
		} else {
			// Reset first so a channel deleted or renamed since the last sync
			// loses its series instead of being pinned at its final value.
			metrics.ResetChannelHealth()
			for _, channel := range channels {
				metrics.SetChannelUp(channel.Id, channel.Name, channel.Enabled)
			}
		}
		if activeKeys, err := model.CountActiveByokKeys(); err != nil {
			common.SysError("metrics: failed to count active BYOK keys: " + err.Error())
		} else {
			metrics.SetByokKeysActive(int(activeKeys))
		}
		time.Sleep(interval)
	}
}
