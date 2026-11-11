package services

import (
	"math"
	"testing"
)

func TestSimulateGrowthPlan_OwnerFigures(t *testing.T) {
	s := DefaultGrowthPlan()
	weeks := SimulateGrowthPlan(s, 12, GrowthDaysPerMonth)
	if len(weeks) != 12 {
		t.Fatalf("got %d weeks", len(weeks))
	}
	w1 := weeks[0]
	if w1.People != 200 {
		t.Errorf("week 1 people = %d, want 200", w1.People)
	}
	// 6,000 takings − 690 fees − 37,000/30.4375 fixed = 4,094.39 a day.
	if math.Abs(w1.ProfitPerDay-4094.39) > 0.01 {
		t.Errorf("week 1 profit/day = %.2f, want 4094.39", w1.ProfitPerDay)
	}
	if w1.NewPeople != 25 {
		t.Errorf("week 1 new people = %d, want 25", w1.NewPeople)
	}
	want := []int{200, 225, 254, 288}
	for i, p := range want {
		if weeks[i].People != p {
			t.Errorf("week %d people = %d, want %d", i+1, weeks[i].People, p)
		}
	}
	for i, w := range weeks {
		t.Logf("week %2d: people %5d  profit/day %8.2f  new %4d", i+1, w.People, w.ProfitPerDay, w.NewPeople)
	}
}

// The owner's hand figure of ~4,077/day comes from 30-day months.
func TestSimulateGrowthPlan_ThirtyDayMonths(t *testing.T) {
	weeks := SimulateGrowthPlan(DefaultGrowthPlan(), 4, 30)
	if math.Round(weeks[0].ProfitPerDay) != 4077 {
		t.Errorf("week 1 profit/day (30-day months) = %.2f, want ≈4077", weeks[0].ProfitPerDay)
	}
	if weeks[0].NewPeople != 25 || weeks[3].People != 288 {
		t.Errorf("30-day months: new %d, week 4 people %d", weeks[0].NewPeople, weeks[3].People)
	}
}

func TestSimulateGrowthPlan_MarketCapAndLosses(t *testing.T) {
	s := DefaultGrowthPlan()
	s.MarketSize = 230
	weeks := SimulateGrowthPlan(s, 4, GrowthDaysPerMonth)
	if weeks[1].People != 225 || weeks[2].People != 230 || weeks[3].People != 230 {
		t.Errorf("market cap not respected: %d %d %d", weeks[1].People, weeks[2].People, weeks[3].People)
	}

	// A plan that loses money never buys homes and never shrinks.
	loss := DefaultGrowthPlan()
	loss.StartPeople = 10
	lw := SimulateGrowthPlan(loss, 3, GrowthDaysPerMonth)
	for _, w := range lw {
		if w.ProfitPerDay >= 0 || w.Reinvest != 0 || w.NewPeople != 0 || w.People != 10 {
			t.Errorf("loss week wrong: %+v", w)
		}
	}
	if len(SimulateGrowthPlan(s, 0, 0)) != 0 {
		t.Error("zero weeks should be empty")
	}
}
