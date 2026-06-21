package billing

import (
	"testing"
	"time"
)

func TestHourlyRate(t *testing.T) {
	r := Rates{CPUHour: 0.01, MemoryGBHour: 0.005, Currency: "USD"}
	// 2 vCPU -> 0.02; 2048 MB = 2 GB -> 0.01; total 0.03/hr.
	if got := r.HourlyRate(2, 2048); got != 0.03 {
		t.Errorf("HourlyRate(2, 2048) = %v, want 0.03", got)
	}
	if got := r.HourlyRate(0, 0); got != 0 {
		t.Errorf("HourlyRate(0,0) = %v, want 0", got)
	}
}

func TestCost(t *testing.T) {
	r := Rates{CPUHour: 0.01, MemoryGBHour: 0.005}
	// 0.03/hr for 2h -> 0.06.
	if got := r.Cost(2, 2048, 2*time.Hour); got != 0.06 {
		t.Errorf("Cost 2h = %v, want 0.06", got)
	}
	// Half an hour bills half (pay-as-you-go).
	if got := r.Cost(2, 2048, 30*time.Minute); got != 0.015 {
		t.Errorf("Cost 30m = %v, want 0.015", got)
	}
	if got := r.Cost(2, 2048, 0); got != 0 {
		t.Errorf("Cost 0 = %v, want 0", got)
	}
	if got := r.Cost(2, 2048, -time.Hour); got != 0 {
		t.Errorf("Cost negative = %v, want 0", got)
	}
}

func TestCostSeconds(t *testing.T) {
	r := Rates{CPUHour: 0.01, MemoryGBHour: 0.005}
	// 3600s == 1h -> 0.03.
	if got := r.CostSeconds(2, 2048, 3600); got != 0.03 {
		t.Errorf("CostSeconds 3600 = %v, want 0.03", got)
	}
	if got := r.CostSeconds(2, 2048, 0); got != 0 {
		t.Errorf("CostSeconds 0 = %v, want 0", got)
	}
}

func TestRound2(t *testing.T) {
	cases := map[float64]float64{
		0.005:      0.01,
		0.014999:   0.01,
		1.235:      1.24,
		0.06000001: 0.06,
	}
	for in, want := range cases {
		if got := Round2(in); got != want {
			t.Errorf("Round2(%v) = %v, want %v", in, got, want)
		}
	}
}
