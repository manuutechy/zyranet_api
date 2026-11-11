package services

import "math"

// The owner's growth plan, as a deterministic week-by-week simulation.
//
// Model (one step per 7-day week):
//   - Week 1 starts with StartPeople homes.
//   - Takings per day   = people × PricePerDay.
//   - Fees per day      = takings × FeePercent% (payment fees + landlord share).
//   - Fixed per day     = (FixedMonthly + CostPerPersonMonthly × people) / days-per-month.
//   - Profit per day    = takings − fees − fixed.
//   - Reinvest pot      = ReinvestPercent% of the week's profit (7 × profit per day),
//     nothing when the week lost money.
//   - New homes         = floor(pot / CostPerNewPerson); they join at the start of
//     the NEXT week. Any money left over is not carried forward.
//   - People never go above MarketSize.

// GrowthDaysPerMonth turns monthly costs into daily ones (365.25 / 12).
const GrowthDaysPerMonth = 30.4375

// GrowthPlanSettings are the owner's plan numbers (per ISP).
type GrowthPlanSettings struct {
	StartDate            string  `json:"plan_start_date"` // YYYY-MM-DD, EAT; week 1 = the Mon–Sun week holding it
	StartPeople          int     `json:"plan_start_people"`
	PricePerDay          float64 `json:"plan_price_per_day"`
	FeePercent           float64 `json:"plan_fee_percent"`
	FixedMonthly         float64 `json:"plan_fixed_monthly"`
	CostPerPersonMonthly float64 `json:"plan_cost_per_person_monthly"`
	ReinvestPercent      float64 `json:"plan_reinvest_percent"`
	CostPerNewPerson     float64 `json:"plan_cost_per_new_person"`
	MarketSize           int     `json:"plan_market_size"`
}

// DefaultGrowthPlan is the owner's starting plan.
func DefaultGrowthPlan() GrowthPlanSettings {
	return GrowthPlanSettings{
		StartPeople:          200,
		PricePerDay:          30,
		FeePercent:           11.5,
		FixedMonthly:         25000,
		CostPerPersonMonthly: 60,
		ReinvestPercent:      90,
		CostPerNewPerson:     1000,
		MarketSize:           10000,
	}
}

// PlanWeek is one simulated week.
type PlanWeek struct {
	Week          int     `json:"week"` // 1-based
	People        int     `json:"people"`
	NewPeople     int     `json:"new_people"` // homes bought with this week's pot (join next week)
	TakingsPerDay float64 `json:"takings_per_day"`
	CostsPerDay   float64 `json:"costs_per_day"`
	ProfitPerDay  float64 `json:"profit_per_day"`
	Takings       float64 `json:"takings"`
	Costs         float64 `json:"costs"`
	Profit        float64 `json:"profit"`
	Reinvest      float64 `json:"reinvest"`
}

// GrowthFixedPerDay is the fixed running cost per day for this many people.
func GrowthFixedPerDay(s GrowthPlanSettings, people float64, daysPerMonth float64) float64 {
	if daysPerMonth <= 0 {
		daysPerMonth = GrowthDaysPerMonth
	}
	return (s.FixedMonthly + s.CostPerPersonMonthly*people) / daysPerMonth
}

// SimulateGrowthPlan runs the plan for `weeks` weeks. daysPerMonth is
// normally GrowthDaysPerMonth; it is a parameter so the model can be checked
// against hand calculations made with 30-day months.
func SimulateGrowthPlan(s GrowthPlanSettings, weeks int, daysPerMonth float64) []PlanWeek {
	if weeks <= 0 {
		return []PlanWeek{}
	}
	out := make([]PlanWeek, 0, weeks)
	people := s.StartPeople
	if people < 0 {
		people = 0
	}
	if s.MarketSize > 0 && people > s.MarketSize {
		people = s.MarketSize
	}
	for w := 1; w <= weeks; w++ {
		takings := float64(people) * s.PricePerDay
		costs := takings*s.FeePercent/100 + GrowthFixedPerDay(s, float64(people), daysPerMonth)
		profitDay := takings - costs
		pw := PlanWeek{
			Week:          w,
			People:        people,
			TakingsPerDay: takings,
			CostsPerDay:   costs,
			ProfitPerDay:  profitDay,
			Takings:       takings * 7,
			Costs:         costs * 7,
			Profit:        profitDay * 7,
		}
		if pw.Profit > 0 {
			pw.Reinvest = pw.Profit * s.ReinvestPercent / 100
		}
		if s.CostPerNewPerson > 0 && pw.Reinvest > 0 {
			pw.NewPeople = int(math.Floor(pw.Reinvest/s.CostPerNewPerson + 1e-9))
		}
		if s.MarketSize > 0 && people+pw.NewPeople > s.MarketSize {
			pw.NewPeople = s.MarketSize - people
			if pw.NewPeople < 0 {
				pw.NewPeople = 0
			}
		}
		out = append(out, pw)
		people += pw.NewPeople
	}
	return out
}
