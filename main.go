package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gocolly/colly"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "ubee_uvw320b"

var (
	version   string
	revision  string
	branch    string
	buildUser string
	buildDate string
)

// Exporter represents an instance of the Netgear cable modem exporter.
type Exporter struct {
	host        string
	credentials map[string]string

	mu sync.Mutex

	// Exporter metrics.
	totalScrapes prometheus.Counter
	scrapeErrors prometheus.Counter

	// Downstream metrics.
	dsChannelSNR        *prometheus.Desc
	dsChannelPower      *prometheus.Desc
	dsCorrectableErrs   *prometheus.Desc
	dsUncorrectableErrs *prometheus.Desc

	// Upstream metrics.
	usChannelPower      *prometheus.Desc
	usChannelSymbolRate *prometheus.Desc

	// Ziggo metrics.
	reportedTimeouts *prometheus.Desc
	modemUptime      *prometheus.Desc
}

// NewExporter returns an instance of Exporter configured with the modem's
// address, admin username and password.
func NewExporter(addr, username, password string) *Exporter {
	var (
		dsLabelNames = []string{"channel", "lock_status", "modulation", "frequency"}
		usLabelNames = []string{"channel", "lock_status", "channel_type", "frequency"}
	)

	return &Exporter{
		// Modem access details.
		host:        addr,
		credentials: map[string]string{"loginUsername": username, "loginPassword": password},

		// Collection metrics.
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "status_scrapes_total",
			Help:      "Total number of scrapes of the modem status page.",
		}),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "status_scrape_errors_total",
			Help:      "Total number of failed scrapes of the modem status page.",
		}),

		// Downstream metrics.
		dsChannelSNR: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "downstream_channel", "snr_db"),
			"Downstream channel signal to noise ratio in dB.",
			dsLabelNames, nil,
		),
		dsChannelPower: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "downstream_channel", "power_dbmv"),
			"Downstream channel power in dBmV.",
			dsLabelNames, nil,
		),
		dsCorrectableErrs: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "downstream_channel", "correctable_errors_total"),
			"Downstream channel correctable errors.",
			nil, nil,
		),
		dsUncorrectableErrs: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "downstream_channel", "uncorrectable_errors_total"),
			"Downstream channel uncorrectable errors.",
			nil, nil,
		),

		// Upstream metrics.
		usChannelPower: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "upstream_channel", "power_dbmv"),
			"Upstream channel power in dBmV.",
			usLabelNames, nil,
		),
		usChannelSymbolRate: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "upstream_channel", "symbol_rate"),
			"Upstream channel symbol rate per second",
			usLabelNames, nil,
		),

		// Ziggo metrics.
		reportedTimeouts: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "timeouts_total"),
			"Timeouts as reported by Ziggo",
			[]string{"name"}, nil,
		),
		modemUptime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "uptime_seconds"),
			"Reported uptime of the modem in seconds.",
			[]string{"firmware"}, nil,
		),
	}
}

// Describe returns Prometheus metric descriptions for the exporter metrics.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// Exporter metrics.
	ch <- e.totalScrapes.Desc()
	ch <- e.scrapeErrors.Desc()
	// Downstream metrics.
	ch <- e.dsChannelSNR
	ch <- e.dsChannelPower
	ch <- e.dsCorrectableErrs
	ch <- e.dsUncorrectableErrs
	// Upstream metrics.
	ch <- e.usChannelPower
	ch <- e.usChannelSymbolRate
	// Ziggo metrics.
	ch <- e.reportedTimeouts
	ch <- e.modemUptime
}

type UptimeInfo struct {
	uptime   time.Duration
	firmware string
}

func (e *Exporter) CollectDocsis(ch chan<- prometheus.Metric, dom *goquery.Selection) {
	// Downstream Bonded Channels
	dom.Find("table:nth-of-type(2) tr").Each(func(i int, row *goquery.Selection) {
		if i < 2 {
			return // row 0 and 1 are headers
		}

		var (
			channel    string
			lockStatus string
			modulation string
			freqMHz    string
			snr        float64
			power      float64
		)

		row.Find("td").Each(func(j int, col *goquery.Selection) {
			text := strings.TrimSpace(col.Text())

			switch j {
			case 0:
				channel = text
			case 1:
				lockStatus = text
			case 2:
				modulation = text
			case 3:
				{
					var freqHZ float64
					fmt.Sscanf(text, "%f Hz", &freqHZ)
					freqMHz = fmt.Sprintf("%0.2f Mhz", freqHZ/1e6)
				}
			case 4:
				fmt.Sscanf(text, "%f dBmV", &power)
			case 5:
				fmt.Sscanf(text, "%f dB", &snr)
			}
		})

		labels := []string{channel, lockStatus, modulation, freqMHz}

		ch <- prometheus.MustNewConstMetric(e.dsChannelSNR, prometheus.GaugeValue, snr, labels...)
		ch <- prometheus.MustNewConstMetric(e.dsChannelPower, prometheus.GaugeValue, power, labels...)
	})

	// Downstream Bonded Channels - Correctables/Uncorrectables
	dom.Find("table:nth-of-type(3) tr:nth-of-type(2) td").Each(func(i int, col *goquery.Selection) {
		var value int
		fmt.Sscanf(strings.TrimSpace(col.Text()), "%d", &value)

		switch i {
		case 0:
			ch <- prometheus.MustNewConstMetric(e.dsCorrectableErrs, prometheus.CounterValue, float64(value))
		case 1:
			ch <- prometheus.MustNewConstMetric(e.dsUncorrectableErrs, prometheus.CounterValue, float64(value))
		}
	})

	// Upstream Bonded Channels
	dom.Find("table:nth-of-type(4) tr").Each(func(i int, row *goquery.Selection) {
		if i < 2 {
			return // row 0 and 1 are headers
		}

		var (
			channel     string
			lockStatus  string
			channelType string
			symbolRate  float64
			freqMHz     string
			power       float64
		)

		row.Find("td").Each(func(j int, col *goquery.Selection) {
			text := strings.TrimSpace(col.Text())

			switch j {
			case 0:
				channel = text
			case 1:
				lockStatus = text
			case 2:
				channelType = text
			case 3:
				{
					fmt.Sscanf(text, "%f Ksym/sec", &symbolRate)
					symbolRate = symbolRate * 1000 // convert to sym/sec
				}
			case 4:
				{
					var freqHZ float64
					fmt.Sscanf(text, "%f Hz", &freqHZ)
					freqMHz = fmt.Sprintf("%0.2f Mhz", freqHZ/1e6)
				}
			case 5:
				fmt.Sscanf(text, "%f dBmV", &power)
			}
		})

		labels := []string{channel, lockStatus, channelType, freqMHz}

		ch <- prometheus.MustNewConstMetric(e.usChannelPower, prometheus.GaugeValue, power, labels...)
		ch <- prometheus.MustNewConstMetric(e.usChannelSymbolRate, prometheus.GaugeValue, symbolRate, labels...)
	})

	// Counters
	dom.Find("table:nth-of-type(5) tr").Each(func(i int, row *goquery.Selection) {
		if i < 1 {
			return // row 0 is a header
		}

		var (
			name    string
			counter int
		)

		row.Find("td").Each(func(j int, col *goquery.Selection) {
			text := strings.TrimSpace(col.Text())

			switch j {
			case 0:
				name = text
			case 1:
				{
					fmt.Sscanf(text, "%d", &counter)
				}
			}
		})

		ch <- prometheus.MustNewConstMetric(e.reportedTimeouts, prometheus.CounterValue, float64(counter), name)
	})
}

func (e *Exporter) CollectStatus(out *UptimeInfo, dom *goquery.Selection) {
	// #main_page > div.table_data > table > tbody > tr:nth-child(2) > td:nth-child(2)
	// <td>1 days 02h:22m:53s</td>
	// <td>0 days 00h:00m:36s</td>

	dom.Find("table:nth-of-type(1) tr:nth-child(2) > td:nth-child(2)").Each(func(_ int, sel *goquery.Selection) {
		var (
			days    int
			hours   int8
			minutes int8
			seconds int8
		)

		fmt.Sscanf(strings.TrimSpace(sel.Text()),
			"%d days %02dh:%02dm:%02ds",
			&days,
			&hours,
			&minutes,
			&seconds,
		)

		out.uptime = (time.Duration(days*24+int(hours)) * time.Hour) +
			(time.Duration(minutes) * time.Minute) +
			(time.Duration(seconds) * time.Second)
	})
}

func (e *Exporter) CollectFirmware(out *UptimeInfo, dom *goquery.Selection) {
	// #main_page > div.table_data > table > tbody > tr:nth-child(3) > td:nth-child(2)
	dom.Find("table tr:nth-child(3) > td:nth-child(2)").Each(func(_ int, sel *goquery.Selection) {
		out.firmware = strings.TrimSpace(sel.Text())
	})
}

func ZiggoLoginHandler() func(req *http.Request, via []*http.Request) error {
	// request = POST
	// http://192.168.178.1/goform/loginMR3 -> Location: http://192.168.178.1/loginMR3.asp -> FAILURE
	// http://192.168.178.1/goform/loginMR3 -> Location: http://192.168.178.1/RgHomeMR3.asp -> SUCCESS

	return func(req *http.Request, via []*http.Request) error {
		if via[0].URL.Path == "/goform/loginMR3" {
			switch req.URL.Path {
			case "/RgHomeMR3.asp":
				return nil
			case "/loginMR3.asp":
				return fmt.Errorf("login failed")
			default:
				return fmt.Errorf("unknown login redirect: %s", req.URL)
			}
		} else {
			return http.ErrUseLastResponse
		}
	}
}

// Collect runs our scrape loop returning each Prometheus metric.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.totalScrapes.Inc()

	c := colly.NewCollector()
	c.RedirectHandler = ZiggoLoginHandler()

	uptimeInfo := &UptimeInfo{}

	// OnError callback counts any errors that occur during scraping.
	c.OnError(func(r *colly.Response, err error) {
		log.Printf("scrape failed: %d %s", r.StatusCode, http.StatusText(r.StatusCode))
		e.scrapeErrors.Inc()
	})

	c.OnHTML(".uuzp-contentholder", func(elem *colly.HTMLElement) {
		elem.DOM.Find("#navigation_bar li a.current").Each(func(_ int, selection *goquery.Selection) {
			var mainPage = elem.DOM.Find("#main_page")

			switch strings.TrimSpace(selection.Text()) {
			case "Docsis":
				e.CollectDocsis(ch, mainPage)
			case "Status":
				e.CollectStatus(uptimeInfo, mainPage)
			case "Firmware":
				e.CollectFirmware(uptimeInfo, mainPage)
			}
		})
	})

	e.mu.Lock()

	err := c.Post(fmt.Sprintf("http://%s/goform/loginMR3", e.host), e.credentials)
	if err == nil {
		c.Visit(fmt.Sprintf("http://%s/BasicCmState.asp", e.host))
		c.Visit(fmt.Sprintf("http://%s/BasicStatus.asp", e.host))
		c.Visit(fmt.Sprintf("http://%s/BasicFirmware.asp", e.host))

		ch <- prometheus.MustNewConstMetric(
			e.modemUptime,
			prometheus.GaugeValue,
			float64(uptimeInfo.uptime.Seconds()),
			uptimeInfo.firmware,
		)
	}

	e.totalScrapes.Collect(ch)
	e.scrapeErrors.Collect(ch)
	e.mu.Unlock()
}

func main() {
	var (
		configFile  = flag.String("config.file", "netgear_cm_exporter.yml", "Path to configuration file.")
		showVersion = flag.Bool("version", false, "Print version information.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("netgear_cm_exporter version=%s revision=%s branch=%s buildUser=%s buildDate=%s\n",
			version, revision, branch, buildUser, buildDate)
		os.Exit(0)
	}

	config, err := NewConfigFromFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}

	exporter := NewExporter(config.Modem.Address, config.Modem.Username, config.Modem.Password)

	prometheus.MustRegister(exporter)

	http.Handle(config.Telemetry.MetricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, config.Telemetry.MetricsPath, http.StatusMovedPermanently)
	})

	log.Printf("exporter listening on %s", config.Telemetry.ListenAddress)
	if err := http.ListenAndServe(config.Telemetry.ListenAddress, nil); err != nil {
		log.Fatalf("failed to start netgear exporter: %s", err)
	}
}
