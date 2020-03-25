// Copyright 2016 The Prometheus Authors
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

package discovery

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	sd_config "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/discovery/targetgroup"

	"github.com/prometheus/prometheus/discovery/azure"
	"github.com/prometheus/prometheus/discovery/consul"
	"github.com/prometheus/prometheus/discovery/dns"
	"github.com/prometheus/prometheus/discovery/ec2"
	"github.com/prometheus/prometheus/discovery/file"
	"github.com/prometheus/prometheus/discovery/gce"
	"github.com/prometheus/prometheus/discovery/kubernetes"
	"github.com/prometheus/prometheus/discovery/marathon"
	"github.com/prometheus/prometheus/discovery/openstack"
	"github.com/prometheus/prometheus/discovery/triton"
	"github.com/prometheus/prometheus/discovery/zookeeper"
)

var (
	// 定义监控指标项

	// 配置项服务发现失败数量
	failedConfigs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "prometheus_sd_failed_configs",
			Help: "Current number of service discovery configurations that failed to load.",
		},
		[]string{"name"},
	)

	// 配置项服务发现对应target数量
	discoveredTargets = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "prometheus_sd_discovered_targets",
			Help: "Current number of discovered targets.",
		},
		[]string{"name", "config"},
	)

	// 配置变更次数
	receivedUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_sd_received_updates_total",
			Help: "Total number of update events received from the SD providers.",
		},
		[]string{"name"},
	)

	// 配置更新延迟通知次数
	delayedUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_sd_updates_delayed_total",
			Help: "Total number of update events that couldn't be sent immediately.",
		},
		[]string{"name"},
	)
	// 配置更新通知次数
	sentUpdates = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_sd_updates_total",
			Help: "Total number of update events sent to the SD consumers.",
		},
		[]string{"name"},
	)
)

func init() {
	// 注册 监控指标
	prometheus.MustRegister(failedConfigs, discoveredTargets, receivedUpdates, delayedUpdates, sentUpdates)
}

// Discoverer provides information about target groups. It maintains a set
// of sources from which TargetGroups can originate. Whenever a discovery provider
// detects a potential change, it sends the TargetGroup through its channel.
//
// Discoverer does not know if an actual change happened.
// It does guarantee that it sends the new TargetGroup whenever a change happens.
//
// Discoverers should initially send a full set of all discoverable TargetGroups.
// 定义配置发现接口
type Discoverer interface {
	// Run hands a channel to the discovery provider (Consul, DNS etc) through which it can send
	// updated target groups.
	// Must returns if the context gets canceled. It should not close the update
	// channel on returning.
	Run(ctx context.Context, up chan<- []*targetgroup.Group)
}

type poolKey struct {
	setName  string
	provider string
}

// provider holds a Discoverer instance, its configuration and its subscribers.
type provider struct {
	name   string
	d      Discoverer
	subs   []string
	config interface{}
}

// NewManager is the Discovery Manager constructor.
func NewManager(ctx context.Context, logger log.Logger, options ...func(*Manager)) *Manager {
	// 创建Manager结构体指针
	if logger == nil {
		logger = log.NewNopLogger()
	}
	mgr := &Manager{
		logger:         logger,
		syncCh:         make(chan map[string][]*targetgroup.Group),      // 用于通知使用者获取配置信息
		targets:        make(map[poolKey]map[string]*targetgroup.Group), // [jobname/config-index, configtype-index] => target.Source => Group
		discoverCancel: []context.CancelFunc{},
		ctx:            ctx,
		updatert:       5 * time.Second,        // 触发配置信息给使用者周期
		triggerSend:    make(chan struct{}, 1), // 用于通知配置已变化
	}
	for _, option := range options {
		option(mgr) //设置管理器属性, 如Name
	}
	return mgr
}

// Name sets the name of the manager.
// 管理器属性设置生成函数
func Name(n string) func(*Manager) {
	return func(m *Manager) {
		m.mtx.Lock()
		defer m.mtx.Unlock()
		m.name = n
	}
}

// Manager maintains a set of discovery providers and sends each update to a map channel.
// Targets are grouped by the target set name.
// 管理器
type Manager struct {
	logger         log.Logger
	name           string
	mtx            sync.RWMutex
	ctx            context.Context
	discoverCancel []context.CancelFunc // 子发现例程上下文取消函数

	// Some Discoverers(eg. k8s) send only the updates for a given target group
	// so we use map[tg.Source]*targetgroup.Group to know which group to update.
	targets map[poolKey]map[string]*targetgroup.Group // 目标信息, [jobname/config-index, configtype-index] => target.Source => Group
	// providers keeps track of SD providers.
	providers []*provider // 服务发现代理对象集合
	// The sync channel sends the updates as a map where the key is the job value from the scrape config.
	syncCh chan map[string][]*targetgroup.Group // 用于通知使用者获取配置信息

	// How long to wait before sending updates to the channel. The variable
	// should only be modified in unit tests.
	updatert time.Duration // 触发配置信息给使用者周期

	// The triggerSend channel signals to the manager that new updates have been received from providers.
	triggerSend chan struct{} // 用于通知配置已变化
}

// Run starts the background processing
// 启动管理器
func (m *Manager) Run() error {
	go m.sender() // 启动例程监控triggerSend管道, 当配置发生变更, 处理数据到syncCh
	for range m.ctx.Done() {
		m.cancelDiscoverers() //服务结束, 通知所有服务发退出
		return m.ctx.Err()
	}
	return nil
}

// SyncCh returns a read only channel used by all the clients to receive target updates.
// 获取配置目标数据
func (m *Manager) SyncCh() <-chan map[string][]*targetgroup.Group {
	return m.syncCh
}

// ApplyConfig removes all running discovery providers and starts new ones using the provided config.
// 应用配置
func (m *Manager) ApplyConfig(cfg map[string]sd_config.ServiceDiscoveryConfig) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	// 从监控指标信息中移除不存在的目标项(jobname/config-index)
	for pk := range m.targets {
		if _, ok := cfg[pk.setName]; !ok {
			discoveredTargets.DeleteLabelValues(m.name, pk.setName)
		}
	}

	// 停止所有服务发现
	m.cancelDiscoverers()

	m.targets = make(map[poolKey]map[string]*targetgroup.Group)
	m.providers = nil
	m.discoverCancel = nil

	failedCount := 0
	for name, scfg := range cfg {

		// 注册服务发现
		failedCount += m.registerProviders(scfg, name)

		// 初始化监控指标信息 服务发现target(name, config)数量
		discoveredTargets.WithLabelValues(m.name, name).Set(0)
	}

	// 设置监控指标信息 配置项服务发现失败数量
	failedConfigs.WithLabelValues(m.name).Set(float64(failedCount))

	// 启动服务发现, 进行变更监控
	for _, prov := range m.providers {
		m.startProvider(m.ctx, prov)
	}

	return nil
}

// StartCustomProvider is used for sdtool. Only use this if you know what you're doing.
func (m *Manager) StartCustomProvider(ctx context.Context, name string, worker Discoverer) {
	p := &provider{
		name: name,
		d:    worker,
		subs: []string{name},
	}
	m.providers = append(m.providers, p)
	m.startProvider(ctx, p)
}

func (m *Manager) startProvider(ctx context.Context, p *provider) {
	// 启动服务发现适配器监控配置变更
	level.Debug(m.logger).Log("msg", "Starting provider", "provider", p.name, "subs", fmt.Sprintf("%v", p.subs))

	ctx, cancel := context.WithCancel(ctx)
	updates := make(chan []*targetgroup.Group) // 目标组管道

	m.discoverCancel = append(m.discoverCancel, cancel)

	go p.d.Run(ctx, updates)      // 启动服务发现监控, 当发生变更将数据发送到updates管道，通知更新服务
	go m.updater(ctx, p, updates) //更新目标信息targets
}

func (m *Manager) updater(ctx context.Context, p *provider, updates chan []*targetgroup.Group) {
	// 更新目标信息
	for {
		select {
		case <-ctx.Done():
			return
		case tgs, ok := <-updates:
			// 更新监控指标 配置变更次数
			receivedUpdates.WithLabelValues(m.name).Inc()

			// 当管道关闭进行记录, 主要针对静态配置
			if !ok {
				level.Debug(m.logger).Log("msg", "discoverer channel closed", "provider", p.name)
				return
			}

			// 更新目标
			for _, s := range p.subs {
				m.updateGroup(poolKey{setName: s, provider: p.name}, tgs)
			}

			// 发送消息, 通知使用例程更新配置
			select {
			case m.triggerSend <- struct{}{}:
			default:
			}
		}
	}
}

func (m *Manager) sender() {
	// 当配置发生变更, 处理数据到syncCh, 通知使用者更新配置
	ticker := time.NewTicker(m.updatert)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done(): //退出
			return
		case <-ticker.C: // Some discoverers send updates too often so we throttle these with the ticker.
			select {
			case <-m.triggerSend: //配置变更

				// 设置监控指标信息 发送更新通知数量
				sentUpdates.WithLabelValues(m.name).Inc()
				select {
				case m.syncCh <- m.allGroups(): // 格式化目标信息到管道
				default:
					// 设置监控指标信息 发送更新更新延迟通知数量
					delayedUpdates.WithLabelValues(m.name).Inc()
					level.Debug(m.logger).Log("msg", "discovery receiver's channel was full so will retry the next cycle")
					select {
					case m.triggerSend <- struct{}{}:
					default:
					}
				}
			default:
			}
		}
	}
}

func (m *Manager) cancelDiscoverers() {
	// 通知服务发现关闭
	for _, c := range m.discoverCancel {
		c()
	}
}

func (m *Manager) updateGroup(poolKey poolKey, tgs []*targetgroup.Group) {
	// 更新目标信息
	m.mtx.Lock()
	defer m.mtx.Unlock()
	for _, tg := range tgs {
		if tg != nil { // Some Discoverers send nil target group so need to check for it to avoid panics.
			if _, ok := m.targets[poolKey]; !ok {
				m.targets[poolKey] = make(map[string]*targetgroup.Group)
			}
			m.targets[poolKey][tg.Source] = tg
		}
	}
}

func (m *Manager) allGroups() map[string][]*targetgroup.Group {
	// 格式化目标信息
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	tSets := map[string][]*targetgroup.Group{}
	for pkey, tsets := range m.targets {
		var n int
		for _, tg := range tsets {
			// Even if the target group 'tg' is empty we still need to send it to the 'Scrape manager'
			// to signal that it needs to stop all scrape loops for this target set.
			tSets[pkey.setName] = append(tSets[pkey.setName], tg)
			n += len(tg.Targets)
		}

		// 更新监控指标信息 服务发现target(name, config)数量
		discoveredTargets.WithLabelValues(m.name, pkey.setName).Set(float64(n))
	}
	return tSets
}

// registerProviders returns a number of failed SD config.
func (m *Manager) registerProviders(cfg sd_config.ServiceDiscoveryConfig, setName string) int {
	// 注册服务发现
	var (
		failedCount int
		added       bool
	)

	// 添加各种注册器到Manager
	add := func(cfg interface{}, newDiscoverer func() (Discoverer, error)) {
		// 获取服务发现配置结构体名
		t := reflect.TypeOf(cfg).String()
		for _, p := range m.providers {
			// 比较配置信息是否已经存在, 若存在更新服务发现所属主题(jobname)
			if reflect.DeepEqual(cfg, p.config) {
				p.subs = append(p.subs, setName)
				added = true
				return
			}
		}

		// 创建服务发现
		d, err := newDiscoverer()
		if err != nil {
			level.Error(m.logger).Log("msg", "Cannot create service discovery", "err", err, "type", t)
			failedCount++
			return
		}

		// 创建服务发现代理对象
		provider := provider{
			name:   fmt.Sprintf("%s/%d", t, len(m.providers)), // 服务发现配置类型+index
			d:      d,                                         // 服务发现
			config: cfg,                                       // 服务发现配置
			subs:   []string{setName},                         // 服务发现所属主题(jobname)
		}
		m.providers = append(m.providers, &provider) //添加服务发现代理对象到集合
		added = true
	}

	// 处理DNS服务发现
	for _, c := range cfg.DNSSDConfigs {
		add(c, func() (Discoverer, error) {
			return dns.NewDiscovery(*c, log.With(m.logger, "discovery", "dns")), nil
		})
	}

	// 处理本地文件服务发现
	for _, c := range cfg.FileSDConfigs {
		add(c, func() (Discoverer, error) {
			return file.NewDiscovery(c, log.With(m.logger, "discovery", "file")), nil
		})
	}

	// 处理Consul服务发现
	for _, c := range cfg.ConsulSDConfigs {
		add(c, func() (Discoverer, error) {
			return consul.NewDiscovery(c, log.With(m.logger, "discovery", "consul"))
		})
	}
	// 处理Marathon服务发现
	for _, c := range cfg.MarathonSDConfigs {
		add(c, func() (Discoverer, error) {
			return marathon.NewDiscovery(*c, log.With(m.logger, "discovery", "marathon"))
		})
	}
	// 处理K8s服务发现
	for _, c := range cfg.KubernetesSDConfigs {
		add(c, func() (Discoverer, error) {
			return kubernetes.New(log.With(m.logger, "discovery", "k8s"), c)
		})
	}

	// 处理Serverset服务发现
	for _, c := range cfg.ServersetSDConfigs {
		add(c, func() (Discoverer, error) {
			return zookeeper.NewServersetDiscovery(c, log.With(m.logger, "discovery", "zookeeper"))
		})
	}

	// 处理Nerve服务发现
	for _, c := range cfg.NerveSDConfigs {
		add(c, func() (Discoverer, error) {
			return zookeeper.NewNerveDiscovery(c, log.With(m.logger, "discovery", "nerve"))
		})
	}
	// 处理EC2服务发现
	for _, c := range cfg.EC2SDConfigs {
		add(c, func() (Discoverer, error) {
			return ec2.NewDiscovery(c, log.With(m.logger, "discovery", "ec2")), nil
		})
	}

	// 处理OpenStack服务发现
	for _, c := range cfg.OpenstackSDConfigs {
		add(c, func() (Discoverer, error) {
			return openstack.NewDiscovery(c, log.With(m.logger, "discovery", "openstack"))
		})
	}

	// 处理GCE服务发现
	for _, c := range cfg.GCESDConfigs {
		add(c, func() (Discoverer, error) {
			return gce.NewDiscovery(*c, log.With(m.logger, "discovery", "gce"))
		})
	}

	// 处理Azure服务发现
	for _, c := range cfg.AzureSDConfigs {
		add(c, func() (Discoverer, error) {
			return azure.NewDiscovery(c, log.With(m.logger, "discovery", "azure")), nil
		})
	}

	// 处理Triton服务发现
	for _, c := range cfg.TritonSDConfigs {
		add(c, func() (Discoverer, error) {
			return triton.New(log.With(m.logger, "discovery", "triton"), c)
		})
	}

	// 处理静态配置
	if len(cfg.StaticConfigs) > 0 {
		add(setName, func() (Discoverer, error) {
			return &StaticProvider{TargetGroups: cfg.StaticConfigs}, nil
		})
	}
	if !added {
		// Add an empty target group to force the refresh of the corresponding
		// scrape pool and to notify the receiver that this target set has no
		// current targets.
		// It can happen because the combined set of SD configurations is empty
		// or because we fail to instantiate all the SD configurations.
		add(setName, func() (Discoverer, error) {
			return &StaticProvider{TargetGroups: []*targetgroup.Group{{}}}, nil
		})
	}
	return failedCount
}

// StaticProvider holds a list of target groups that never change.
// 处理服务发现静态配置
type StaticProvider struct {
	TargetGroups []*targetgroup.Group
}

// Run implements the Worker interface.
func (sd *StaticProvider) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	// We still have to consider that the consumer exits right away in which case
	// the context will be canceled.
	select {
	case ch <- sd.TargetGroups:
	case <-ctx.Done():
	}
	close(ch)
}
