package prometheus

import (
	"errors"
	"strconv"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/crosbymichael/cgroups"
	metrics "github.com/docker/go-metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var ErrAlreadyCollected = errors.New("cgroup is already being collected")

// New registers the Collector with the provided namespace and returns it so
// that cgroups can be added for collection
func New(ns *metrics.Namespace) *Collector {
	// add machine cpus and memory info
	c := &Collector{
		ns:      ns,
		cgroups: make(map[string]cgroups.Cgroup),
	}
	c.metrics = append(c.metrics, pidMetrics...)
	c.metrics = append(c.metrics, cpuMetrics...)
	c.metrics = append(c.metrics, memoryMetrics...)
	c.metrics = append(c.metrics, hugetlbMetrics...)
	c.metrics = append(c.metrics, blkioMetrics...)
	ns.Add(c)
	return c
}

// Collector provides the ability to collect container stats and export
// them in the prometheus format
type Collector struct {
	mu sync.RWMutex

	cgroups map[string]cgroups.Cgroup
	ns      *metrics.Namespace
	metrics []*metric
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range c.metrics {
		ch <- m.desc(c.ns)
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	wg := &sync.WaitGroup{}
	for id, cg := range c.cgroups {
		wg.Add(1)
		go c.collect(id, cg, ch, wg)
	}
	c.mu.RUnlock()
	wg.Wait()
}

func (c *Collector) collect(id string, cg cgroups.Cgroup, ch chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()
	stats, err := cg.Stat()
	if err != nil {
		logrus.WithError(err).Errorf("stat cgroup %s", id)
		return
	}
	for _, m := range c.metrics {
		m.collect(id, stats, c.ns, ch)
	}
}

// Add adds the provided cgroup and id so that metrics are collected and exported
func (c *Collector) Add(id string, cg cgroups.Cgroup) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cgroups[id]; ok {
		return ErrAlreadyCollected
	}
	c.cgroups[id] = cg
	return nil
}

func blkioValues(l []cgroups.BlkioEntry) []value {
	var out []value
	for _, e := range l {
		out = append(out, value{
			v: float64(e.Value),
			l: []string{e.Op, strconv.FormatUint(e.Major, 10), strconv.FormatUint(e.Minor, 10)},
		})
	}
	return out
}
