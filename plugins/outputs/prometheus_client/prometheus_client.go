package prometheus_client

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var invalidNameCharRE = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// SampleID uniquely identifies a Sample
type SampleID string

// Sample represents the current value of a series.
type Sample struct {
	// Labels are the Prometheus labels.
	Labels map[string]string
	// Value is the value in the Prometheus output. Only one of these will populated.
	Value          float64
	HistogramValue map[float64]uint64
	SummaryValue   map[float64]float64
	// Histograms and Summaries need a count and a sum
	Count uint64
	Sum   float64
	// Expiration is the deadline that this Sample is valid until.
	Expiration time.Time
}

// MetricFamily contains the data required to build valid prometheus Metrics.
type MetricFamily struct {
	// Samples are the Sample belonging to this MetricFamily.
	Samples map[SampleID]*Sample
	// Type of the Value.
	PromValueType prometheus.ValueType
	// Need the telegraf ValueType because there isn't a Prometheus ValueType
	// representing Histogram or Summary
	TelegrafValueType telegraf.ValueType
	// LabelSet is the label counts for all Samples.
	LabelSet map[string]int
}

type PrometheusClient struct {
	Listen             string
	ExpirationInterval internal.Duration `toml:"expiration_interval"`
	Path               string            `toml:"path"`
	CollectorsExclude  []string          `toml:"collectors_exclude"`

	server *http.Server

	sync.Mutex
	// fam is the non-expired MetricFamily by Prometheus metric name.
	fam map[string]*MetricFamily
	// now returns the current time.
	now func() time.Time
}

var sampleConfig = `
  ## Address to listen on
  # listen = ":9273"

  ## Interval to expire metrics and not deliver to prometheus, 0 == no expiration
  # expiration_interval = "60s"

  ## Collectors to enable, valid entries are "gocollector" and "process".
  ## If unset, both are enabled.
  collectors_exclude = ["gocollector", "process"]
`

func (p *PrometheusClient) Start() error {
	prometheus.Register(p)

	for _, collector := range p.CollectorsExclude {
		switch collector {
		case "gocollector":
			prometheus.Unregister(prometheus.NewGoCollector())
		case "process":
			prometheus.Unregister(prometheus.NewProcessCollector(os.Getpid(), ""))
		default:
			return fmt.Errorf("unrecognized collector %s", collector)
		}
	}

	if p.Listen == "" {
		p.Listen = "localhost:9273"
	}

	if p.Path == "" {
		p.Path = "/metrics"
	}

	mux := http.NewServeMux()
	mux.Handle(p.Path, promhttp.HandlerFor(
		prometheus.DefaultGatherer,
		promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}))

	p.server = &http.Server{
		Addr:    p.Listen,
		Handler: mux,
	}

	go func() {
		if err := p.server.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				log.Printf("E! Error creating prometheus metric endpoint, err: %s\n",
					err.Error())
			}
		}
	}()
	return nil
}

func (p *PrometheusClient) Stop() {
	// plugin gets cleaned up in Close() already.
}

func (p *PrometheusClient) Connect() error {
	// This service output does not need to make any further connections
	return nil
}

func (p *PrometheusClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	err := p.server.Shutdown(ctx)
	prometheus.Unregister(p)
	return err
}

func (p *PrometheusClient) SampleConfig() string {
	return sampleConfig
}

func (p *PrometheusClient) Description() string {
	return "Configuration for the Prometheus client to spawn"
}

// Implements prometheus.Collector
func (p *PrometheusClient) Describe(ch chan<- *prometheus.Desc) {
	prometheus.NewGauge(prometheus.GaugeOpts{Name: "Dummy", Help: "Dummy"}).Describe(ch)
}

// Expire removes Samples that have expired.
func (p *PrometheusClient) Expire() {
	now := p.now()
	for name, family := range p.fam {
		for key, sample := range family.Samples {
			if p.ExpirationInterval.Duration != 0 && now.After(sample.Expiration) {
				for k, _ := range sample.Labels {
					family.LabelSet[k]--
				}
				delete(family.Samples, key)

				if len(family.Samples) == 0 {
					delete(p.fam, name)
				}
			}
		}
	}
}

// Collect implements prometheus.Collector
func (p *PrometheusClient) Collect(ch chan<- prometheus.Metric) {
	p.Lock()
	defer p.Unlock()

	p.Expire()

	for name, family := range p.fam {
		// Get list of all labels on MetricFamily
		var labelNames []string
		for k, v := range family.LabelSet {
			if v > 0 {
				labelNames = append(labelNames, k)
			}
		}
		desc := prometheus.NewDesc(name, "Telegraf collected metric", labelNames, nil)

		for _, sample := range family.Samples {
			// Get labels for this sample; unset labels will be set to the
			// empty string
			var labels []string
			for _, label := range labelNames {
				v := sample.Labels[label]
				labels = append(labels, v)
			}

			var metric prometheus.Metric
			var err error
			switch family.TelegrafValueType {
			case telegraf.Summary:
				metric, err = prometheus.NewConstSummary(desc, sample.Count, sample.Sum, sample.SummaryValue, labels...)
				if err != nil {
					log.Printf("E! Error creating prometheus metric, "+
						"key: %s, labels: %v,\nerr: %s\n",
						name, labels, err.Error())
				}
			case telegraf.Histogram:
				metric, err = prometheus.NewConstHistogram(desc, sample.Count, sample.Sum, sample.HistogramValue, labels...)
				if err != nil {
					log.Printf("E! Error creating prometheus metric, "+
						"key: %s, labels: %v,\nerr: %s\n",
						name, labels, err.Error())
				}
			default:
				metric, err = prometheus.NewConstMetric(desc, family.PromValueType, sample.Value, labels...)
				if err != nil {
					log.Printf("E! Error creating prometheus metric, "+
						"key: %s, labels: %v,\nerr: %s\n",
						name, labels, err.Error())
				}
			}

			ch <- metric
		}
	}
}

func sanitize(value string) string {
	return invalidNameCharRE.ReplaceAllString(value, "_")
}

func valueType(tt telegraf.ValueType) prometheus.ValueType {
	switch tt {
	case telegraf.Counter:
		return prometheus.CounterValue
	case telegraf.Gauge:
		return prometheus.GaugeValue
	default:
		return prometheus.UntypedValue
	}
}

// CreateSampleID creates a SampleID based on the tags of a telegraf.Metric.
func CreateSampleID(tags map[string]string) SampleID {
	pairs := make([]string, 0, len(tags))
	for k, v := range tags {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(pairs)
	return SampleID(strings.Join(pairs, ","))
}

func (p *PrometheusClient) Write(metrics []telegraf.Metric) error {
	p.Lock()
	defer p.Unlock()

	now := p.now()

	for _, point := range metrics {
		tags := point.Tags()
		vt := valueType(point.Type())
		sampleID := CreateSampleID(tags)

		labels := make(map[string]string)
		for k, v := range tags {
			labels[sanitize(k)] = v
		}

		switch point.Type() {
		case telegraf.Summary:
			var mname string
			var sum float64
			var count uint64
			value := make(map[float64]float64)
			for fn, fv := range point.Fields() {
				log.Printf("field: %+v, value: %+v", fn, fv)
				switch fn {
				case "sum":
					sum = fv.(float64)
				case "count":
					count = uint64(fv.(float64))
				default:
					limit, err := strconv.ParseFloat(fn, 64)
					if err == nil {
						value[limit] = fv.(float64)
					}
				}
			}
			sample := &Sample{
				Labels:       labels,
				SummaryValue: value,
				Count:        count,
				Sum:          sum,
				Expiration:   now.Add(p.ExpirationInterval.Duration),
			}
			mname = sanitize(point.Name())

			var fam *MetricFamily
			var ok bool
			if fam, ok = p.fam[mname]; !ok {
				fam = &MetricFamily{
					Samples:           make(map[SampleID]*Sample),
					PromValueType:     vt,
					TelegrafValueType: point.Type(),
					LabelSet:          make(map[string]int),
				}
				p.fam[mname] = fam
			} else {
				// Metrics can be untyped even though the corresponding plugin
				// creates them with a type.  This happens when the metric was
				// transferred over the network in a format that does not
				// preserve value type and received using an input such as a
				// queue consumer.  To avoid issues we automatically upgrade
				// value type from untyped to a typed metric.
				if fam.PromValueType == prometheus.UntypedValue {
					fam.PromValueType = vt
				}

				if vt != prometheus.UntypedValue && fam.PromValueType != vt {
					// Don't return an error since this would be a permanent error
					log.Printf("Mixed ValueType for measurement %q; dropping point", point.Name())
					break
				}
			}

			for k, _ := range sample.Labels {
				fam.LabelSet[k]++
			}

			fam.Samples[sampleID] = sample
		case telegraf.Histogram:
			var mname string
			var sum float64
			var count uint64
			value := make(map[float64]uint64)
			for fn, fv := range point.Fields() {
				log.Printf("field: %+v, value: %+v", fn, fv)
				switch fn {
				case "sum":
					sum = fv.(float64)
				case "count":
					count = uint64(fv.(float64))
				default:
					limit, err := strconv.ParseFloat(fn, 64)
					if err == nil {
						value[limit] = uint64(fv.(float64))
					}
				}
			}
			sample := &Sample{
				Labels:         labels,
				HistogramValue: value,
				Count:          count,
				Sum:            sum,
				Expiration:     now.Add(p.ExpirationInterval.Duration),
			}
			mname = sanitize(point.Name())

			var fam *MetricFamily
			var ok bool
			if fam, ok = p.fam[mname]; !ok {
				fam = &MetricFamily{
					Samples:           make(map[SampleID]*Sample),
					PromValueType:     vt,
					TelegrafValueType: point.Type(),
					LabelSet:          make(map[string]int),
				}
				p.fam[mname] = fam
			} else {
				// Metrics can be untyped even though the corresponding plugin
				// creates them with a type.  This happens when the metric was
				// transferred over the network in a format that does not
				// preserve value type and received using an input such as a
				// queue consumer.  To avoid issues we automatically upgrade
				// value type from untyped to a typed metric.
				if fam.PromValueType == prometheus.UntypedValue {
					fam.PromValueType = vt
				}

				if vt != prometheus.UntypedValue && fam.PromValueType != vt {
					// Don't return an error since this would be a permanent error
					log.Printf("Mixed ValueType for measurement %q; dropping point", point.Name())
					break
				}
			}

			for k, _ := range sample.Labels {
				fam.LabelSet[k]++
			}
			fam.Samples[sampleID] = sample
		default:
			for fn, fv := range point.Fields() {
				// Ignore string and bool fields.
				var value float64
				switch fv := fv.(type) {
				case int64:
					value = float64(fv)
				case float64:
					value = fv
				default:
					continue
				}

				sample := &Sample{
					Labels:     labels,
					Value:      value,
					Expiration: now.Add(p.ExpirationInterval.Duration),
				}

				// Special handling of value field; supports passthrough from
				// the prometheus input.
				var mname string
				switch point.Type() {
				case telegraf.Counter:
					if fn == "counter" {
						mname = sanitize(point.Name())
					}
				case telegraf.Gauge:
					if fn == "gauge" {
						mname = sanitize(point.Name())
					}
				}
				if mname == "" {
					if fn == "value" {
						mname = sanitize(point.Name())
					} else {
						mname = sanitize(fmt.Sprintf("%s_%s", point.Name(), fn))
					}
				}

				var fam *MetricFamily
				var ok bool
				if fam, ok = p.fam[mname]; !ok {
					fam = &MetricFamily{
						Samples:           make(map[SampleID]*Sample),
						PromValueType:     vt,
						TelegrafValueType: point.Type(),
						LabelSet:          make(map[string]int),
					}
					p.fam[mname] = fam
				} else {
					// Metrics can be untyped even though the corresponding plugin
					// creates them with a type.  This happens when the metric was
					// transferred over the network in a format that does not
					// preserve value type and received using an input such as a
					// queue consumer.  To avoid issues we automatically upgrade
					// value type from untyped to a typed metric.
					if fam.PromValueType == prometheus.UntypedValue {
						fam.PromValueType = vt
					}

					if vt != prometheus.UntypedValue && fam.PromValueType != vt {
						// Don't return an error since this would be a permanent error
						log.Printf("Mixed ValueType for measurement %q; dropping point", point.Name())
						break
					}
				}

				for k, _ := range sample.Labels {
					fam.LabelSet[k]++
				}

				fam.Samples[sampleID] = sample
			}
		}
	}
	return nil
}

func init() {
	outputs.Add("prometheus_client", func() telegraf.Output {
		return &PrometheusClient{
			ExpirationInterval: internal.Duration{Duration: time.Second * 60},
			fam:                make(map[string]*MetricFamily),
			now:                time.Now,
		}
	})
}
