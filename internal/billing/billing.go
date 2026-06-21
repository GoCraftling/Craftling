// Package billing prices metered server running time for pay-as-you-go, hourly
// billing (P9). It holds only the pure pricing arithmetic; the metered intervals
// live in the billing repository and the HTTP shape in the handler.
package billing

import (
	"math"
	"time"
)

// Rates is the control plane's pay-as-you-go price list: a per-vCPU-hour and a
// per-GB-of-memory-hour rate, in a single currency. A server's price is linear
// in its spec, so a 2-vCPU / 2-GB server simply costs 2*CPUHour + 2*MemoryGBHour
// per hour of running time.
type Rates struct {
	CPUHour      float64
	MemoryGBHour float64
	Currency     string
}

// HourlyRate is the price of running a server with the given spec for one hour.
func (r Rates) HourlyRate(cpus, memoryMB int) float64 {
	return float64(cpus)*r.CPUHour + (float64(memoryMB)/1024.0)*r.MemoryGBHour
}

// Cost prices a span of running time for a server of the given spec. Billing is
// continuous (pay-as-you-go): a partial hour bills the matching fraction.
func (r Rates) Cost(cpus, memoryMB int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return r.HourlyRate(cpus, memoryMB) * d.Hours()
}

// CostSeconds is Cost for a duration given in seconds, the form the metered
// ledger sums in SQL.
func (r Rates) CostSeconds(cpus, memoryMB int, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return r.HourlyRate(cpus, memoryMB) * (seconds / 3600.0)
}

// Round2 rounds a currency amount to cents, so summed line items don't surface
// floating-point dust in the API.
func Round2(v float64) float64 {
	return math.Round(v*100) / 100
}
