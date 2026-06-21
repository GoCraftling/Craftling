package model

import "time"

// BillingLineItem is one server's metered running cost over a billing period
// (P9). Hours is the summed running time in the period, HourlyRate the price of
// the server's spec per hour, and Cost the resulting charge. Running marks a
// server still on the clock — its cost is accruing as of the response.
type BillingLineItem struct {
	ServerID   string  `json:"server_id"`
	Name       string  `json:"name"`
	CPUs       int     `json:"cpus"`
	MemoryMB   int     `json:"memory_mb"`
	Hours      float64 `json:"hours"`
	HourlyRate float64 `json:"hourly_rate"`
	Cost       float64 `json:"cost"`
	Running    bool    `json:"running"`
}

// BillingSummary is a user's pay-as-you-go bill for a period: the per-server
// line items, the total charge, and the current burn rate (the summed hourly
// price of everything still running). CPUHour/MemoryGBHour echo the active price
// list so a client can render the rate card without a second call.
type BillingSummary struct {
	Currency     string            `json:"currency"`
	PeriodStart  time.Time         `json:"period_start"`
	CPUHour      float64           `json:"cpu_hour"`
	MemoryGBHour float64           `json:"memory_gb_hour"`
	Items        []BillingLineItem `json:"items"`
	TotalCost    float64           `json:"total_cost"`
	HourlyRate   float64           `json:"hourly_rate"`
}
