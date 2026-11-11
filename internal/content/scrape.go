package content

import (
	"time"

	"go.yaml.in/yaml/v3"
)

// defaultScrape is the scrape interval Prometheus uses when its
// configuration gives none.
const defaultScrape = time.Minute

// ScrapeInterval is the global scrape interval in a Prometheus
// configuration, as /api/v1/status/config gives it; ok is false when the
// configuration can't be read.
func ScrapeInterval(config string) (d time.Duration, ok bool) {
	var c struct {
		Global struct {
			ScrapeInterval string `yaml:"scrape_interval"`
		} `yaml:"global"`
	}
	if err := yaml.Unmarshal([]byte(config), &c); err != nil {
		return 0, false
	}
	if c.Global.ScrapeInterval == "" {
		return defaultScrape, true
	}
	d, ok = ParseDuration(c.Global.ScrapeInterval)
	return d, ok && d > 0
}
