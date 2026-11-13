package main

import (
	"math"
	"testing"
)

// near compares within one part in a million (absolute below 1), so a monthly figure in the
// thousands is held to the same precision as an hourly one.
func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// planHourly is the node pool's price from the documented formula, not a rounded constant.
const planHourly = 3 * (0.051584 + 30*0.052/730)

func TestClusterHourlyMatchesThePlan(t *testing.T) {
	m := mustConfig(t).Cost
	// 3 × ($0.051584 + 30 GB × $0.052 / 730 h) = $0.161163/h, $117.65 a month: PLAN.md's ~$118.
	if got := m.clusterHourly(); !near(got, planHourly) || math.Abs(got-0.161163) > 1e-6 {
		t.Errorf("clusterHourly = %.8f", got)
	}
	if got := m.clusterHourly() * m.HoursPerMonth; math.Abs(got-117.649) > 0.01 {
		t.Errorf("monthly = %.3f", got)
	}
	if got := m.allocatableCPU(); !near(got, 5.79) {
		t.Errorf("allocatable = %v", got)
	}
}

func TestPriceCapacity(t *testing.T) {
	m := mustConfig(t).Cost
	cpu := 0.579 // a tenth of the allocatable CPU
	r := priceCapacity(m, 10, &cpu)
	per1K := planHourly * 1000 / 10 // $16.12 an hour at 10 RPS
	if !near(r.Per1KRPSHourly, per1K) || !near(r.Per1KRPSMonthly, per1K*730) {
		t.Errorf("per 1K RPS = %v / %v, want %v / %v", r.Per1KRPSHourly, r.Per1KRPSMonthly, per1K, per1K*730)
	}
	if r.GeneratorShare == nil || !near(*r.GeneratorShare, 0.1) || !near(*r.NetPer1KRPSHourly, per1K*0.9) || !near(*r.NetPer1KRPSMonthly, per1K*730*0.9) {
		t.Errorf("net = %+v", r)
	}
	if r.Currency != "USD" || r.RPS != 10 || !near(r.ClusterMonthly, planHourly*730) {
		t.Errorf("result = %+v", r)
	}

	if r := priceCapacity(m, 10, nil); r.NetPer1KRPSHourly != nil {
		t.Error("net price without generator CPU")
	}
	tooMuch := 6.0
	if r := priceCapacity(m, 10, &tooMuch); r.NetPer1KRPSHourly != nil {
		t.Error("net price with the generator using more than the cluster")
	}
}
