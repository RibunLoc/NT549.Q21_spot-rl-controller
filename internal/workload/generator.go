package workload

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

var hourlyProfile = [24]float64{
	0.3, 0.2, 0.2, 0.2, 0.3, 0.5, // 00-05: đêm
	0.7, 0.9, 1.1, 1.5, 1.6, 1.4, // 06-11: sáng
	0.2, 0.9, 1.8, 2.0, 1.8, 1.5, // 12-17: chiều
	1.2, 1.0, 0.8, 0.6, 0.5, 0.4, // 18-23: tối
}

type Generator struct {
	baseRate        float64
	spikeProb       float64
	spikeMultiplier float64
	spikeRemaining  int
	currentStep     int
	jobCounter      int
	rng             *rand.Rand // "math/rand"
}

func NewGenerator(baseRate float64, seed int64) *Generator {
	return &Generator{
		baseRate:        baseRate,
		spikeProb:       0,
		spikeMultiplier: 3.0,
		spikeRemaining:  0,
		currentStep:     0,
		jobCounter:      0,
		rng:             rand.New(rand.NewSource(seed)),
	}
}

func (g *Generator) Step() []Job {
	hour := g.currentStep % 24
	rate := g.baseRate * hourlyProfile[hour]
	nJobs := poissonSample(g.rng, rate)

	jobs := []Job{}

	for i := 0; i < nJobs; i++ {
		job := Job{
			JobID:       fmt.Sprintf("job-%d", g.jobCounter),
			Type:        g.randomType(), // light/medium/heavy
			DurationSec: 60,
			ArrivedAt:   time.Now(),
		}
		jobs = append(jobs, job) // thêm job vào slice
		g.jobCounter++
	}
	g.currentStep++

	return jobs
}

func (g *Generator) CurrentStep() int {
	return g.currentStep
}

func poissonSample(rng *rand.Rand, rate float64) int {
	L := math.Exp(-rate)
	k := 0
	p := 1.0

	for p > L {
		k++
		p = p * rng.Float64()
	}

	return k - 1
}

func (g *Generator) randomType() string {
	result := g.rng.Float64() // [0-1)

	if result < 0.60 {
		return "light"
	} else if result < 0.90 {
		return "medium"
	}

	return "heavy"
}
