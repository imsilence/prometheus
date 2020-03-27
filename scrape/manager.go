// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scrape

import (
	"encoding"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"
)

var targetMetadataCache = newMetadataMetricsCollector()

// MetadataMetricsCollector is a Custom Collector for the metadata cache metrics.
type MetadataMetricsCollector struct {
	CacheEntries *prometheus.Desc
	CacheBytes   *prometheus.Desc

	scrapeManager *Manager
}

func newMetadataMetricsCollector() *MetadataMetricsCollector {
	return &MetadataMetricsCollector{
		CacheEntries: prometheus.NewDesc(
			"prometheus_target_metadata_cache_entries",
			"Total number of metric metadata entries in the cache",
			[]string{"scrape_job"},
			nil,
		),
		CacheBytes: prometheus.NewDesc(
			"prometheus_target_metadata_cache_bytes",
			"The number of bytes that are currently used for storing metric metadata in the cache",
			[]string{"scrape_job"},
			nil,
		),
	}
}

func (mc *MetadataMetricsCollector) registerManager(m *Manager) {
	mc.scrapeManager = m
}

// Describe sends the metrics descriptions to the channel.
func (mc *MetadataMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- mc.CacheEntries
	ch <- mc.CacheBytes
}

// Collect creates and sends the metrics for the metadata cache.
func (mc *MetadataMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	if mc.scrapeManager == nil {
		return
	}

	for tset, targets := range mc.scrapeManager.TargetsActive() {
		var size, length int
		for _, t := range targets {
			size += t.MetadataSize()
			length += t.MetadataLength()
		}

		ch <- prometheus.MustNewConstMetric(
			mc.CacheEntries,
			prometheus.GaugeValue,
			float64(length),
			tset,
		)

		ch <- prometheus.MustNewConstMetric(
			mc.CacheBytes,
			prometheus.GaugeValue,
			float64(size),
			tset,
		)
	}
}

// Appendable returns an Appender.
type Appendable interface {
	Appender() (storage.Appender, error)
}

// NewManager is the Manager constructor
// 创建采集器
func NewManager(logger log.Logger, app Appendable) *Manager {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	m := &Manager{
		append:        app,
		logger:        logger,
		scrapeConfigs: make(map[string]*config.ScrapeConfig), // 存储每个job对应的配置信息
		scrapePools:   make(map[string]*scrapePool),          //存储每个job对应的采集池
		graceShut:     make(chan struct{}),
		triggerReload: make(chan struct{}, 1),
	}
	targetMetadataCache.registerManager(m) // 注册指标信息

	return m
}

// Manager maintains a set of scrape pools and manages start/stop cycles
// when receiving new target groups form the discovery manager.
// 采集器
type Manager struct {
	logger    log.Logger
	append    Appendable    // 数据记录器
	graceShut chan struct{} // 结束管道

	jitterSeed    uint64                          // Global jitterSeed seed is used to spread scrape workload across HA setup.
	mtxScrape     sync.Mutex                      // Guards the fields below.
	scrapeConfigs map[string]*config.ScrapeConfig // job对应配置
	scrapePools   map[string]*scrapePool          // job对应采集池
	targetSets    map[string][]*targetgroup.Group //job对应采集目标

	triggerReload chan struct{} // 重新加载管道
}

// Run receives and saves target set updates and triggers the scraping loops reloading.
// Reloading happens in the background so that it doesn't block receiving targets updates.
// 启动采集
func (m *Manager) Run(tsets <-chan map[string][]*targetgroup.Group) error {
	go m.reloader() // 采集池更新
	for {
		select {
		case ts := <-tsets:
			m.updateTsets(ts) // 采集目标更新

			select {
			case m.triggerReload <- struct{}{}: // 触发采集池更新
			default:
			}

		case <-m.graceShut:
			return nil
		}
	}
}

func (m *Manager) reloader() {
	// 更新并启动采集池
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.graceShut:
			return
		case <-ticker.C:
			select {
			case <-m.triggerReload:
				m.reload()
			case <-m.graceShut:
				return
			}
		}
	}
}

func (m *Manager) reload() {
	// 更新并启动采集池
	m.mtxScrape.Lock()
	var wg sync.WaitGroup
	for setName, groups := range m.targetSets {
		if _, ok := m.scrapePools[setName]; !ok { // 新增采集池
			scrapeConfig, ok := m.scrapeConfigs[setName] // 获取job配置
			if !ok {
				level.Error(m.logger).Log("msg", "error reloading target set", "err", "invalid config id:"+setName)
				continue
			}
			// 创建采集吃
			sp, err := newScrapePool(scrapeConfig, m.append, m.jitterSeed, log.With(m.logger, "scrape_pool", setName))
			if err != nil {
				level.Error(m.logger).Log("msg", "error creating new scrape pool", "err", err, "scrape_pool", setName)
				continue
			}
			m.scrapePools[setName] = sp // 存放缓存池
		}

		wg.Add(1)
		// Run the sync in parallel as these take a while and at high load can't catch up.
		// 更新 采集池 采集目标
		go func(sp *scrapePool, groups []*targetgroup.Group) {
			sp.Sync(groups)
			wg.Done()
		}(m.scrapePools[setName], groups)

	}
	m.mtxScrape.Unlock()
	wg.Wait()
}

// setJitterSeed calculates a global jitterSeed per server relying on extra label set.
// 生成采集器唯一标识
func (m *Manager) setJitterSeed(labels labels.Labels) error {
	h := fnv.New64a()
	hostname, err := getFqdn()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(h, "%s%s", hostname, labels.String()); err != nil {
		return err
	}
	m.jitterSeed = h.Sum64()
	return nil
}

// Stop cancels all running scrape pools and blocks until all have exited.
func (m *Manager) Stop() {
	m.mtxScrape.Lock()
	defer m.mtxScrape.Unlock()

	for _, sp := range m.scrapePools {
		sp.stop()
	}
	close(m.graceShut)
}

// 更新采集配置
func (m *Manager) updateTsets(tsets map[string][]*targetgroup.Group) {
	m.mtxScrape.Lock()
	m.targetSets = tsets
	m.mtxScrape.Unlock()
}

// ApplyConfig resets the manager's target providers and job configurations as defined by the new cfg.
// 应用配置
func (m *Manager) ApplyConfig(cfg *config.Config) error {
	m.mtxScrape.Lock()
	defer m.mtxScrape.Unlock()

	// 更新采集配置
	c := make(map[string]*config.ScrapeConfig)
	for _, scfg := range cfg.ScrapeConfigs {
		c[scfg.JobName] = scfg
	}
	m.scrapeConfigs = c

	// 更新采集器唯一标识
	if err := m.setJitterSeed(cfg.GlobalConfig.ExternalLabels); err != nil {
		return err
	}

	// Cleanup and reload pool if the configuration has changed.
	// 清理配置不存在的采集池或更新采集池配置
	var failed bool
	for name, sp := range m.scrapePools {
		if cfg, ok := m.scrapeConfigs[name]; !ok { // 清理不存在的采集池
			sp.stop() // 停止采集池
			delete(m.scrapePools, name)
		} else if !reflect.DeepEqual(sp.config, cfg) {
			err := sp.reload(cfg) // 更新采集池配置
			if err != nil {
				level.Error(m.logger).Log("msg", "error reloading scrape pool", "err", err, "scrape_pool", name)
				failed = true
			}
		}
	}

	if failed {
		return errors.New("failed to apply the new configuration")
	}
	return nil
}

// TargetsAll returns active and dropped targets grouped by job_name.
func (m *Manager) TargetsAll() map[string][]*Target {
	m.mtxScrape.Lock()
	defer m.mtxScrape.Unlock()

	targets := make(map[string][]*Target, len(m.scrapePools))
	for tset, sp := range m.scrapePools {
		targets[tset] = append(sp.ActiveTargets(), sp.DroppedTargets()...)
	}
	return targets
}

// TargetsActive returns the active targets currently being scraped.
func (m *Manager) TargetsActive() map[string][]*Target {
	m.mtxScrape.Lock()
	defer m.mtxScrape.Unlock()

	var (
		wg  sync.WaitGroup
		mtx sync.Mutex
	)

	targets := make(map[string][]*Target, len(m.scrapePools))
	wg.Add(len(m.scrapePools))
	for tset, sp := range m.scrapePools {
		// Running in parallel limits the blocking time of scrapePool to scrape
		// interval when there's an update from SD.
		go func(tset string, sp *scrapePool) {
			mtx.Lock()
			targets[tset] = sp.ActiveTargets()
			mtx.Unlock()
			wg.Done()
		}(tset, sp)
	}
	wg.Wait()
	return targets
}

// TargetsDropped returns the dropped targets during relabelling.
func (m *Manager) TargetsDropped() map[string][]*Target {
	m.mtxScrape.Lock()
	defer m.mtxScrape.Unlock()

	targets := make(map[string][]*Target, len(m.scrapePools))
	for tset, sp := range m.scrapePools {
		targets[tset] = sp.DroppedTargets()
	}
	return targets
}

// getFqdn returns a FQDN if it's possible, otherwise falls back to hostname.
func getFqdn() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}

	ips, err := net.LookupIP(hostname)
	if err != nil {
		// Return the system hostname if we can't look up the IP address.
		return hostname, nil
	}

	lookup := func(ipStr encoding.TextMarshaler) (string, error) {
		ip, err := ipStr.MarshalText()
		if err != nil {
			return "", err
		}
		hosts, err := net.LookupAddr(string(ip))
		if err != nil || len(hosts) == 0 {
			return "", err
		}
		return hosts[0], nil
	}

	for _, addr := range ips {
		if ip := addr.To4(); ip != nil {
			if fqdn, err := lookup(ip); err == nil {
				return fqdn, nil
			}

		}

		if ip := addr.To16(); ip != nil {
			if fqdn, err := lookup(ip); err == nil {
				return fqdn, nil
			}

		}
	}
	return hostname, nil
}
