package main

// costResult prices the highest sustained rate. All money is in the cost model's currency.
type costResult struct {
	Currency        string  `json:"currency"`
	ClusterHourly   float64 `json:"clusterHourly"`
	ClusterMonthly  float64 `json:"clusterMonthly"`
	RPS             float64 `json:"rps"`
	Per1KRPSHourly  float64 `json:"per1kRpsHourly"`
	Per1KRPSMonthly float64 `json:"per1kRpsMonthly"`
	// The load generator ran on the same nodes. Its share of the allocatable CPU, taken out of the
	// cluster cost, gives an estimate of what the shop alone used. Nil when k6's CPU is unknown.
	GeneratorCPU       *float64 `json:"generatorCpuCores,omitempty"`
	GeneratorShare     *float64 `json:"generatorShare,omitempty"`
	NetPer1KRPSHourly  *float64 `json:"netPer1kRpsHourly,omitempty"`
	NetPer1KRPSMonthly *float64 `json:"netPer1kRpsMonthly,omitempty"`
}

// clusterHourly is the node pool's price per hour: each node's price plus its disk, spread over the
// month's hours.
func (m costModel) clusterHourly() float64 {
	return float64(m.Nodes) * (m.NodeHourly + m.DiskGBPerNode*m.DiskGBMonthly/m.HoursPerMonth)
}

func (m costModel) allocatableCPU() float64 { return float64(m.Nodes) * m.AllocatableCPUPerNode }

// priceCapacity gives the cost of serving 1,000 requests per second at the measured rate, assuming
// cost grows in proportion to load (more of the same nodes). rps must be above 0.
func priceCapacity(m costModel, rps float64, generatorCPU *float64) costResult {
	hourly := m.clusterHourly()
	r := costResult{
		Currency:        m.Currency,
		ClusterHourly:   hourly,
		ClusterMonthly:  hourly * m.HoursPerMonth,
		RPS:             rps,
		Per1KRPSHourly:  hourly * 1000 / rps,
		Per1KRPSMonthly: hourly * m.HoursPerMonth * 1000 / rps,
	}
	if generatorCPU != nil && *generatorCPU >= 0 {
		share := *generatorCPU / m.allocatableCPU()
		if share < 1 {
			netHourly := r.Per1KRPSHourly * (1 - share)
			netMonthly := r.Per1KRPSMonthly * (1 - share)
			cpu := *generatorCPU
			r.GeneratorCPU, r.GeneratorShare = &cpu, &share
			r.NetPer1KRPSHourly, r.NetPer1KRPSMonthly = &netHourly, &netMonthly
		}
	}
	return r
}
