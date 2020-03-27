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

package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/go-openapi/strfmt"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/version"

	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
)

const (
	contentTypeJSON = "application/json"
)

// String constants for instrumentation.
const (
	namespace         = "prometheus"
	subsystem         = "notifications"
	alertmanagerLabel = "alertmanager"
)

var userAgent = fmt.Sprintf("Prometheus/%s", version.Version)

// Alert is a generic representation of an alert in the Prometheus eco-system.
// 告警信息
type Alert struct {
	// Label value pairs for purpose of aggregation, matching, and disposition
	// dispatching. This must minimally include an "alertname" label.
	Labels labels.Labels `json:"labels"`

	// Extra key/value information which does not define alert identity.
	Annotations labels.Labels `json:"annotations"`

	// The known time range for this alert. Both ends are optional.
	StartsAt     time.Time `json:"startsAt,omitempty"`
	EndsAt       time.Time `json:"endsAt,omitempty"`
	GeneratorURL string    `json:"generatorURL,omitempty"`
}

// Name returns the name of the alert. It is equivalent to the "alertname" label.
func (a *Alert) Name() string {
	return a.Labels.Get(labels.AlertName)
}

// Hash returns a hash over the alert. It is equivalent to the alert labels hash.
func (a *Alert) Hash() uint64 {
	return a.Labels.Hash()
}

func (a *Alert) String() string {
	s := fmt.Sprintf("%s[%s]", a.Name(), fmt.Sprintf("%016x", a.Hash())[:7])
	if a.Resolved() {
		return s + "[resolved]"
	}
	return s + "[active]"
}

// Resolved returns true iff the activity interval ended in the past.
func (a *Alert) Resolved() bool {
	return a.ResolvedAt(time.Now())
}

// ResolvedAt returns true iff the activity interval ended before
// the given timestamp.
func (a *Alert) ResolvedAt(ts time.Time) bool {
	if a.EndsAt.IsZero() {
		return false
	}
	return !a.EndsAt.After(ts)
}

// Manager is responsible for dispatching alert notifications to an
// alert manager service.
// 通知器
type Manager struct {
	queue []*Alert
	opts  *Options

	metrics *alertMetrics

	more   chan struct{}
	mtx    sync.RWMutex
	ctx    context.Context
	cancel func()

	alertmanagers map[string]*alertmanagerSet
	logger        log.Logger
}

// Options are the configurable parameters of a Handler.
// 通知器配置选项
type Options struct {
	QueueCapacity  int           // 通知队列容量
	ExternalLabels labels.Labels //
	RelabelConfigs []*relabel.Config
	// Used for sending HTTP requests to the Alertmanager.
	Do func(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error)

	Registerer prometheus.Registerer
}

// 监控信息
type alertMetrics struct {
	latency                 *prometheus.SummaryVec
	errors                  *prometheus.CounterVec
	sent                    *prometheus.CounterVec
	dropped                 prometheus.Counter
	queueLength             prometheus.GaugeFunc
	queueCapacity           prometheus.Gauge
	alertmanagersDiscovered prometheus.GaugeFunc
}

// 创建监控信息, 定义监控指标并进行注册
func newAlertMetrics(r prometheus.Registerer, queueCap int, queueLen, alertmanagersDiscovered func() float64) *alertMetrics {
	m := &alertMetrics{
		latency: prometheus.NewSummaryVec(prometheus.SummaryOpts{
			Namespace:  namespace,
			Subsystem:  subsystem,
			Name:       "latency_seconds",
			Help:       "Latency quantiles for sending alert notifications.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
			[]string{alertmanagerLabel},
		),
		// 针对target发送失败告警数量
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "errors_total",
			Help:      "Total number of errors sending alert notifications.",
		},
			[]string{alertmanagerLabel},
		),
		// 针对target发送告警数量
		sent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "sent_total",
			Help:      "Total number of alerts sent.",
		},
			[]string{alertmanagerLabel},
		),
		// 处理失败的告警数量
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dropped_total",
			Help:      "Total number of alerts dropped due to errors when sending to Alertmanager.",
		}),
		// 当前未通知告警数量
		queueLength: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queue_length",
			Help:      "The number of alert notifications in the queue.",
		}, queueLen),
		// 队列容量
		queueCapacity: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queue_capacity",
			Help:      "The capacity of the alert notifications queue.",
		}),
		//  告警通知配置数量
		alertmanagersDiscovered: prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "prometheus_notifications_alertmanagers_discovered",
			Help: "The number of alertmanagers discovered and active.",
		}, alertmanagersDiscovered),
	}

	m.queueCapacity.Set(float64(queueCap))

	if r != nil {
		r.MustRegister(
			m.latency,
			m.errors,
			m.sent,
			m.dropped,
			m.queueLength,
			m.queueCapacity,
			m.alertmanagersDiscovered,
		)
	}

	return m
}

func do(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req.WithContext(ctx))
}

// NewManager is the manager constructor.
// 创建通知器
func NewManager(o *Options, logger log.Logger) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	if o.Do == nil {
		o.Do = do
	}
	if logger == nil {
		logger = log.NewNopLogger()
	}

	// 创建通知器
	n := &Manager{
		queue:  make([]*Alert, 0, o.QueueCapacity),
		ctx:    ctx,
		cancel: cancel,
		more:   make(chan struct{}, 1),
		opts:   o,
		logger: logger,
	}

	// 通知队列消息数量
	queueLenFunc := func() float64 { return float64(n.queueLen()) }

	// 通知目的数量
	alertmanagersDiscoveredFunc := func() float64 { return float64(len(n.Alertmanagers())) }

	// 通知器监控指标
	n.metrics = newAlertMetrics(
		o.Registerer,
		o.QueueCapacity,
		queueLenFunc,
		alertmanagersDiscoveredFunc,
	)

	return n
}

// ApplyConfig updates the status state as the new config requires.
// 应用配置
func (n *Manager) ApplyConfig(conf *config.Config) error {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	n.opts.ExternalLabels = conf.GlobalConfig.ExternalLabels
	n.opts.RelabelConfigs = conf.AlertingConfig.AlertRelabelConfigs

	amSets := make(map[string]*alertmanagerSet) //

	for k, cfg := range conf.AlertingConfig.AlertmanagerConfigs.ToMap() {
		// 根据每项配置信息生成告警通知集合(每个集合对应一各client, 对应多个URL(target))
		ams, err := newAlertmanagerSet(cfg, n.logger, n.metrics)
		if err != nil {
			return err
		}

		amSets[k] = ams
	}

	n.alertmanagers = amSets

	return nil
}

// 每批次发送告警并发数量
const maxBatchSize = 64

// 获取队列中消息数量
func (n *Manager) queueLen() int {
	n.mtx.RLock()
	defer n.mtx.RUnlock()

	return len(n.queue)
}

// 获取发送告警信息
func (n *Manager) nextBatch() []*Alert {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	var alerts []*Alert

	if len(n.queue) > maxBatchSize {
		alerts = append(make([]*Alert, 0, maxBatchSize), n.queue[:maxBatchSize]...)
		n.queue = n.queue[maxBatchSize:]
	} else {
		alerts = append(make([]*Alert, 0, len(n.queue)), n.queue...)
		n.queue = n.queue[:0]
	}

	return alerts
}

// Run dispatches notifications continuously.
// 启动消息通知
func (n *Manager) Run(tsets <-chan map[string][]*targetgroup.Group) {

	for {
		select {
		case <-n.ctx.Done():
			return
		case ts := <-tsets: // 更新通知目标
			n.reload(ts) // 更新通知目标
		case <-n.more:
		}
		alerts := n.nextBatch() // 获取发送告警信息

		if !n.sendAll(alerts...) { // 发送告警
			n.metrics.dropped.Add(float64(len(alerts))) // 更新处理失败的告警数量
		}
		// If the queue still has items left, kick off the next iteration.
		if n.queueLen() > 0 { // 告警未处理完成
			n.setMore() // 通过管道通知告警处理
		}
	}
}

// 更新通知目标组
func (n *Manager) reload(tgs map[string][]*targetgroup.Group) {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	for id, tgroup := range tgs {
		am, ok := n.alertmanagers[id] //判断告警管理集合信息项是否存在
		if !ok {
			level.Error(n.logger).Log("msg", "couldn't sync alert manager set", "err", fmt.Sprintf("invalid id:%v", id))
			continue
		}
		am.sync(tgroup) // 更新告警管理通知集合url
	}
}

// Send queues the given notification requests for processing.
// Panics if called on a handler that is not running.
// 发送告警到队列中
func (n *Manager) Send(alerts ...*Alert) {
	n.mtx.Lock()
	defer n.mtx.Unlock()

	// Attach external labels before relabelling and sending.
	// 填充告警label信息
	for _, a := range alerts {
		lb := labels.NewBuilder(a.Labels)

		for _, l := range n.opts.ExternalLabels {
			if a.Labels.Get(l.Name) == "" {
				lb.Set(l.Name, l.Value)
			}
		}

		a.Labels = lb.Labels()
	}

	// 根据alerting中配置的alert_relabel_configs处理告警label信息
	alerts = n.relabelAlerts(alerts)
	if len(alerts) == 0 {
		return
	}

	// Queue capacity should be significantly larger than a single alert
	// batch could be.
	// 当告警数量大于队列容量时 只处理最后容量告警
	if d := len(alerts) - n.opts.QueueCapacity; d > 0 {
		alerts = alerts[d:]

		level.Warn(n.logger).Log("msg", "Alert batch larger than queue capacity, dropping alerts", "num_dropped", d)
		n.metrics.dropped.Add(float64(d)) // 更新处理失败的告警数量
	}

	// If the queue is full, remove the oldest alerts in favor
	// of newer ones.
	// 当告警数量不能放入队列时 丢弃队列中n各告警
	if d := (len(n.queue) + len(alerts)) - n.opts.QueueCapacity; d > 0 {
		n.queue = n.queue[d:]

		level.Warn(n.logger).Log("msg", "Alert notification queue full, dropping alerts", "num_dropped", d)
		n.metrics.dropped.Add(float64(d)) // 更新处理失败的告警数量
	}

	// 将告警放入队列
	n.queue = append(n.queue, alerts...)

	// Notify sending goroutine that there are alerts to be processed.
	// 通知发送告警
	n.setMore()
}

// 处理告警label信息
func (n *Manager) relabelAlerts(alerts []*Alert) []*Alert {
	var relabeledAlerts []*Alert

	for _, alert := range alerts {
		// 根据alerting中配置的alert_relabel_configs处理告警label信息
		labels := relabel.Process(alert.Labels, n.opts.RelabelConfigs...)
		if labels != nil {
			alert.Labels = labels
			relabeledAlerts = append(relabeledAlerts, alert)
		}
	}
	return relabeledAlerts
}

// setMore signals that the alert queue has items.
func (n *Manager) setMore() {
	// If we cannot send on the channel, it means the signal already exists
	// and has not been consumed yet.
	select {
	case n.more <- struct{}{}:
	default:
	}
}

// Alertmanagers returns a slice of Alertmanager URLs.
// 获取所有告警通知的URL
func (n *Manager) Alertmanagers() []*url.URL {
	n.mtx.RLock()
	amSets := n.alertmanagers
	n.mtx.RUnlock()

	var res []*url.URL

	for _, ams := range amSets {
		ams.mtx.RLock()
		for _, am := range ams.ams {
			res = append(res, am.url())
		}
		ams.mtx.RUnlock()
	}

	return res
}

// DroppedAlertmanagers returns a slice of Alertmanager URLs.
// 获取所有不可告警通知的URL
func (n *Manager) DroppedAlertmanagers() []*url.URL {
	n.mtx.RLock()
	amSets := n.alertmanagers
	n.mtx.RUnlock()

	var res []*url.URL

	for _, ams := range amSets {
		ams.mtx.RLock()
		for _, dam := range ams.droppedAms {
			res = append(res, dam.url())
		}
		ams.mtx.RUnlock()
	}

	return res
}

// sendAll sends the alerts to all configured Alertmanagers concurrently.
// It returns true if the alerts could be sent successfully to at least one Alertmanager.
// 发送告警
func (n *Manager) sendAll(alerts ...*Alert) bool {
	if len(alerts) == 0 {
		return true
	}

	begin := time.Now()

	// v1Payload and v2Payload represent 'alerts' marshaled for Alertmanager API
	// v1 or v2. Marshaling happens below. Reference here is for caching between
	// for loop iterations.
	var v1Payload, v2Payload []byte

	n.mtx.RLock()
	amSets := n.alertmanagers
	n.mtx.RUnlock()

	var (
		wg         sync.WaitGroup
		numSuccess uint64
	)
	// 遍历告警管理集合
	for _, ams := range amSets {
		var (
			payload []byte
			err     error
		)

		ams.mtx.RLock()

		// 根据告警集合配置版本号处理发送信息
		switch ams.cfg.APIVersion {
		case config.AlertmanagerAPIVersionV1: //v1
			{
				if v1Payload == nil {
					v1Payload, err = json.Marshal(alerts) // 发序列化告警列表
					if err != nil {
						level.Error(n.logger).Log("msg", "Encoding alerts for Alertmanager API v1 failed", "err", err)
						return false
					}
				}

				payload = v1Payload
			}
		case config.AlertmanagerAPIVersionV2: // v2
			{
				if v2Payload == nil {
					// 转化alert为PostableAlerts
					openAPIAlerts := alertsToOpenAPIAlerts(alerts)

					v2Payload, err = json.Marshal(openAPIAlerts) // 发序列化告警列表
					if err != nil {
						level.Error(n.logger).Log("msg", "Encoding alerts for Alertmanager API v2 failed", "err", err)
						return false
					}
				}

				payload = v2Payload
			}
		default:
			{
				level.Error(n.logger).Log(
					"msg", fmt.Sprintf("Invalid Alertmanager API version '%v', expected one of '%v'", ams.cfg.APIVersion, config.SupportedAlertmanagerAPIVersions),
					"err", err,
				)
				return false
			}
		}

		// 发送告警给高级管理集中的所有url
		for _, am := range ams.ams {
			wg.Add(1)

			ctx, cancel := context.WithTimeout(n.ctx, time.Duration(ams.cfg.Timeout))
			defer cancel()

			go func(client *http.Client, url string) {
				// 发送告警
				if err := n.sendOne(ctx, client, url, payload); err != nil {
					level.Error(n.logger).Log("alertmanager", url, "count", len(alerts), "msg", "Error sending alert", "err", err)
					n.metrics.errors.WithLabelValues(url).Inc()
				} else {
					atomic.AddUint64(&numSuccess, 1) //更新发送成功数量
				}
				n.metrics.latency.WithLabelValues(url).Observe(time.Since(begin).Seconds()) // 更新告警通知URL调用延迟时间
				n.metrics.sent.WithLabelValues(url).Add(float64(len(alerts)))               // 更新发送告警数量

				wg.Done()
			}(ams.client, am.url().String())
		}

		ams.mtx.RUnlock()
	}

	wg.Wait()

	return numSuccess > 0
}

func alertsToOpenAPIAlerts(alerts []*Alert) models.PostableAlerts {
	// 转化alert为PostableAlerts
	openAPIAlerts := models.PostableAlerts{}
	for _, a := range alerts {
		start := strfmt.DateTime(a.StartsAt)
		end := strfmt.DateTime(a.EndsAt)
		openAPIAlerts = append(openAPIAlerts, &models.PostableAlert{
			Annotations: labelsToOpenAPILabelSet(a.Annotations),
			EndsAt:      end,
			StartsAt:    start,
			Alert: models.Alert{
				GeneratorURL: strfmt.URI(a.GeneratorURL),
				Labels:       labelsToOpenAPILabelSet(a.Labels),
			},
		})
	}

	return openAPIAlerts
}

func labelsToOpenAPILabelSet(modelLabelSet labels.Labels) models.LabelSet {
	apiLabelSet := models.LabelSet{}
	for _, label := range modelLabelSet {
		apiLabelSet[label.Name] = label.Value
	}

	return apiLabelSet
}

func (n *Manager) sendOne(ctx context.Context, c *http.Client, url string, b []byte) error {
	// 发送告警
	req, err := http.NewRequest("POST", url, bytes.NewReader(b)) // 创建请求对象
	if err != nil {
		return err
	}
	// 设置Header user-agent 及 content-type
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", contentTypeJSON)

	// 发送告警
	resp, err := n.opts.Do(ctx, c, req)
	if err != nil {
		return err
	}
	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	// 判断响应状态结果
	// Any HTTP status 2xx is OK.
	if resp.StatusCode/100 != 2 {
		return errors.Errorf("bad response status %s", resp.Status)
	}

	return nil
}

// Stop shuts down the notification handler.
func (n *Manager) Stop() {
	level.Info(n.logger).Log("msg", "Stopping notification manager...")
	n.cancel()
}

// alertmanager holds Alertmanager endpoint information.
type alertmanager interface {
	url() *url.URL
}

type alertmanagerLabels struct{ labels.Labels }

const pathLabel = "__alerts_path__"

func (a alertmanagerLabels) url() *url.URL {
	return &url.URL{
		Scheme: a.Get(model.SchemeLabel),
		Host:   a.Get(model.AddressLabel),
		Path:   a.Get(pathLabel),
	}
}

// alertmanagerSet contains a set of Alertmanagers discovered via a group of service
// discovery definitions that have a common configuration on how alerts should be sent.
// 告警管理集合
type alertmanagerSet struct {
	cfg    *config.AlertmanagerConfig
	client *http.Client

	metrics *alertMetrics

	mtx        sync.RWMutex
	ams        []alertmanager
	droppedAms []alertmanager
	logger     log.Logger
}

func newAlertmanagerSet(cfg *config.AlertmanagerConfig, logger log.Logger, metrics *alertMetrics) (*alertmanagerSet, error) {
	// 创建告警管理集合
	client, err := config_util.NewClientFromConfig(cfg.HTTPClientConfig, "alertmanager", false) // 创建Client
	if err != nil {
		return nil, err
	}
	s := &alertmanagerSet{
		client:  client,
		cfg:     cfg,
		logger:  logger,
		metrics: metrics,
	}
	return s, nil
}

// sync extracts a deduplicated set of Alertmanager endpoints from a list
// of target groups definitions.
func (s *alertmanagerSet) sync(tgs []*targetgroup.Group) {
	// 更新告警管理集合(url)
	allAms := []alertmanager{}
	allDroppedAms := []alertmanager{}

	for _, tg := range tgs {
		ams, droppedAms, err := alertmanagerFromGroup(tg, s.cfg)
		if err != nil {
			level.Error(s.logger).Log("msg", "Creating discovered Alertmanagers failed", "err", err)
			continue
		}
		// 所有可通知的告警管理
		allAms = append(allAms, ams...)

		// 所有不可通知的告警管理(无label)
		allDroppedAms = append(allDroppedAms, droppedAms...)
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()
	// Set new Alertmanagers and deduplicate them along their unique URL.
	s.ams = []alertmanager{}
	s.droppedAms = []alertmanager{}
	s.droppedAms = append(s.droppedAms, allDroppedAms...)

	// 对告警目标通过url进行去重
	seen := map[string]struct{}{}

	for _, am := range allAms {
		us := am.url().String()
		if _, ok := seen[us]; ok {
			continue
		}

		// This will initialize the Counters for the AM to 0.
		// 初始化 各目标 告警发送 和告警发送失败 指标数量
		s.metrics.sent.WithLabelValues(us)
		s.metrics.errors.WithLabelValues(us)

		seen[us] = struct{}{}
		s.ams = append(s.ams, am)
	}
}

func postPath(pre string, v config.AlertmanagerAPIVersion) string {
	// 处理告警通知URL
	alertPushEndpoint := fmt.Sprintf("/api/%v/alerts", string(v))
	return path.Join("/", pre, alertPushEndpoint)
}

// alertmanagerFromGroup extracts a list of alertmanagers from a target group
// and an associated AlertmanagerConfig.
func alertmanagerFromGroup(tg *targetgroup.Group, cfg *config.AlertmanagerConfig) ([]alertmanager, []alertmanager, error) {
	var res []alertmanager
	var droppedAlertManagers []alertmanager

	for _, tlset := range tg.Targets {
		// 获取所有label target中配置的label(地址),group中配置的label, scheme, path
		lbls := make([]labels.Label, 0, len(tlset)+2+len(tg.Labels))

		for ln, lv := range tlset {
			lbls = append(lbls, labels.Label{Name: string(ln), Value: string(lv)})
		}
		// Set configured scheme as the initial scheme label for overwrite.
		// 告警通知协议
		lbls = append(lbls, labels.Label{Name: model.SchemeLabel, Value: cfg.Scheme})
		// 处理告警通知PATH
		lbls = append(lbls, labels.Label{Name: pathLabel, Value: postPath(cfg.PathPrefix, cfg.APIVersion)})

		// Combine target labels with target group labels.
		// 处理group中配置的label
		for ln, lv := range tg.Labels {
			if _, ok := tlset[ln]; !ok {
				lbls = append(lbls, labels.Label{Name: string(ln), Value: string(lv)})
			}
		}

		// 根据alarting.alartmanager.relabel_configs配置处理label
		lset := relabel.Process(labels.New(lbls...), cfg.RelabelConfigs...)
		if lset == nil {
			// 无label告警通知管理数量
			droppedAlertManagers = append(droppedAlertManagers, alertmanagerLabels{lbls})
			continue
		}

		lb := labels.NewBuilder(lset)

		// addPort checks whether we should add a default port to the address.
		// If the address is not valid, we don't append a port either.
		// 判断通知地址是否包含端口信息
		addPort := func(s string) bool {
			// If we can split, a port exists and we don't have to add one.
			if _, _, err := net.SplitHostPort(s); err == nil {
				return false
			}
			// If adding a port makes it valid, the previous error
			// was not due to an invalid address and we can append a port.
			_, _, err := net.SplitHostPort(s + ":1234")
			return err == nil
		}
		// 获取通知地址
		addr := lset.Get(model.AddressLabel)
		// If it's an address with no trailing port, infer it based on the used scheme.
		// 当通知地址不包含端口信息根据协议类型拼写默认端口号
		if addPort(addr) {
			// Addresses reaching this point are already wrapped in [] if necessary.
			switch lset.Get(model.SchemeLabel) {
			case "http", "":
				addr = addr + ":80"
			case "https":
				addr = addr + ":443"
			default:
				return nil, nil, errors.Errorf("invalid scheme: %q", cfg.Scheme)
			}
			lb.Set(model.AddressLabel, addr)
		}

		// 验证地址格式为ip:port
		if err := config.CheckTargetAddress(model.LabelValue(addr)); err != nil {
			return nil, nil, err
		}

		// Meta labels are deleted after relabelling. Other internal labels propagate to
		// the target which decides whether they will be part of their label set.
		// 删除内置label(__meta_开头)
		for _, l := range lset {
			if strings.HasPrefix(l.Name, model.MetaLabelPrefix) {
				lb.Del(l.Name)
			}
		}

		res = append(res, alertmanagerLabels{lset})
	}
	return res, droppedAlertManagers, nil
}
