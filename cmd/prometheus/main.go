// Copyright 2015 The Prometheus Authors
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

// The main package for the Prometheus server executable.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	conntrack "github.com/mwitkow/go-conntrack"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/version"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
	"k8s.io/klog"

	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	sd_config "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/pkg/logging"
	"github.com/prometheus/prometheus/pkg/relabel"
	prom_runtime "github.com/prometheus/prometheus/pkg/runtime"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/scrape"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/tsdb"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/prometheus/prometheus/web"
)

var (
	// 定义监控指标项
	/*
		指标项类型: counter, gauge, histogram, summary
		counter: 表示只增不减的指标信息, 可重置为0. 尝试用_total作为后缀.
		gauge: 表示可增可减的指标信息, 反应当前系统状态.
		histogram, summary: 用于统计和分析样本分布情况, 根据范围进行分组统计. 需要指标记录次数(以_count作为后缀)和指标项总和(以_sum作为后缀)
		histogram直接反应区间[0, end)样本的数量, summary直接反应的样本分位数信息
	*/

	// 最后一次配置是否成功, 1: 成功, 0: 失败
	configSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "prometheus_config_last_reload_successful",
		Help: "Whether the last configuration reload attempt was successful.",
	})

	// 最后一次配置成功时间
	configSuccessTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "prometheus_config_last_reload_success_timestamp_seconds",
		Help: "Timestamp of the last successful configuration reload.",
	})

	// 默认tsdb保留数据的时间, 15d
	defaultRetentionString   = "15d"
	defaultRetentionDuration model.Duration
)

func init() {

	// 定义并注册 监控指标
	// prometheus编译信息, 包含信息: 分支, go版本信息, prometheus版本, commit hash信息
	prometheus.MustRegister(version.NewCollector("prometheus"))

	var err error
	defaultRetentionDuration, err = model.ParseDuration(defaultRetentionString)
	if err != nil {
		panic(err)
	}
}

func main() {
	if os.Getenv("DEBUG") != "" {
		runtime.SetBlockProfileRate(20)
		runtime.SetMutexProfileFraction(20)
	}

	var (
		// tsdb保留数据的时间
		oldFlagRetentionDuration model.Duration // 解析命令行参数storage.tsdb.retention
		newFlagRetentionDuration model.Duration // 解析命令行参数storage.tsdb.retention.time
	)

	// 定义prometheus配置信息
	cfg := struct {
		configFile string // 配置文件, 默认: prometheus.yml

		localStoragePath    string           // tsdb存储目录, 默认: data/
		notifier            notifier.Options // 通知配置, 用于alartmanager api调用
		notifierTimeout     model.Duration   // [未使用]通知超时时间, 解析命令行参数alertmanager.timeout, 默认10s
		forGracePeriod      model.Duration   // 告警恢复时间, 解析命令行参数rules.alert.for-grace-period, 默认10m
		outageTolerance     model.Duration   // TODO: prometheus source, 参数含义&用途, 解析命令行参数rules.alert.for-outage-tolerance, 默认1h
		resendDelay         model.Duration   // 告警静默时间, 解析命令行参数rules.alert.resend-delay, 默认1m
		web                 web.Options      // web配置
		tsdb                tsdb.Options     // tsdb配置
		lookbackDelta       model.Duration   // TODO: prometheus source, 参数含义&用途, 解析命令行参数 query.lookback-delta, 默认5m
		webTimeout          model.Duration   // web读取客户端数据超时时间, 解析命令行参数 web.read-timeout, 默认5m
		queryTimeout        model.Duration   // TODO: prometheus source, 确认, promql查询超时时间, 解析命令行参数query.timeout, 默认2m
		queryConcurrency    int              // TODO: prometheus source, 确认, promql查询并发限制, 解析命令行参数query.max-concurrency, 默认20
		queryMaxSamples     int              // TODO: prometheus source, 确认, promql查询数据限制, 解析命令行参数query.max-samples, 默认50000000
		RemoteFlushDeadline model.Duration   // TODO: prometheus source,  参数含义&用途, remote??存储时间, 解析命令行参数storage.remote.flush-deadline, 默认1m

		prometheusURL   string // prometheus访问地址, 解析命令行参数web.external-url
		corsRegexString string // prometheus跨域请求允许来源, 解析命令行参数web.cors.origin, 默认/*

		promlogConfig promlog.Config // 日志配置
	}{ // 配置值信息
		notifier: notifier.Options{ // 通知配置
			Registerer: prometheus.DefaultRegisterer, // prometheus指标注册器
		},
		web: web.Options{ //web配置
			Registerer: prometheus.DefaultRegisterer, // TODO: prometheus source,  参数含义&用途. prometheus指标注册器
			Gatherer:   prometheus.DefaultGatherer,   // TODO: prometheus source,  参数含义&用途. prometheus指标注册器
		},
		promlogConfig: promlog.Config{}, // 日志配置
	}

	// 定义命令行解析器
	a := kingpin.New(filepath.Base(os.Args[0]), "The Prometheus monitoring server")

	// 指定版本信息
	a.Version(version.Print("prometheus"))

	// 指定帮助命令
	a.HelpFlag.Short('h')

	// 设置prometheus配置文件, 解析命令行参数config.file到cfg.configFile, 默认prometheus.yml
	a.Flag("config.file", "Prometheus configuration file path.").
		Default("prometheus.yml").StringVar(&cfg.configFile)

	// 设置web访问监听IP和端口, 解析命令行参数web.listen-address到cfg.web.ListenAddress, 默认0.0.0.0:9000
	a.Flag("web.listen-address", "Address to listen on for UI, API, and telemetry.").
		Default("0.0.0.0:9090").StringVar(&cfg.web.ListenAddress)

	// 设置web读取客户端数据超时时间, 解析命令行参数web.read-timeout到cfg.webTimeout, 默认5m
	a.Flag("web.read-timeout",
		"Maximum duration before timing out read of the request, and closing idle connections.").
		Default("5m").SetValue(&cfg.webTimeout)

	// 设置web最大连接数量, 解析命令行参数web.max-connections到cfg.web.MaxConnections, 默认512
	a.Flag("web.max-connections", "Maximum number of simultaneous connections.").
		Default("512").IntVar(&cfg.web.MaxConnections)

	// 设置web访问地址, 解析命令行参数web.external-url到cfg.prometheusURL
	a.Flag("web.external-url",
		"The URL under which Prometheus is externally reachable (for example, if Prometheus is served via a reverse proxy). Used for generating relative and absolute links back to Prometheus itself. If the URL has a path portion, it will be used to prefix all HTTP endpoints served by Prometheus. If omitted, relevant URL components will be derived automatically.").
		PlaceHolder("<URL>").StringVar(&cfg.prometheusURL)

	// 设置web路由前缀, 解析命令行参数web.route-prefix到cfg.web.RoutePrefix
	a.Flag("web.route-prefix",
		"Prefix for the internal routes of web endpoints. Defaults to path of --web.external-url.").
		PlaceHolder("<path>").StringVar(&cfg.web.RoutePrefix)

	// 设置web静态文件目录, 解析命令行参数web.user-assets到cfg.web.UserAssetsPath
	a.Flag("web.user-assets", "Path to static asset directory, available at /user.").
		PlaceHolder("<path>").StringVar(&cfg.web.UserAssetsPath)

	// 设置是否启用web对prometheus进行退出和重新加载操作, 解析命令行参数web.enable-lifecycle到cfg.web.EnableLifecycle, 默认false
	a.Flag("web.enable-lifecycle", "Enable shutdown and reload via HTTP request.").
		Default("false").BoolVar(&cfg.web.EnableLifecycle)

	// TODO: prometheus source, 用途, 设置是否启用web admin api, 解析命令行参数web.enable-admin-api到cfg.web.EnableAdminAPI, 默认false
	a.Flag("web.enable-admin-api", "Enable API endpoints for admin control actions.").
		Default("false").BoolVar(&cfg.web.EnableAdminAPI)

	// 设置web模板目录, 解析命令行参数web.console.templates到cfg.web.ConsoleTemplatesPath, 默认consoles
	a.Flag("web.console.templates", "Path to the console template directory, available at /consoles.").
		Default("consoles").StringVar(&cfg.web.ConsoleTemplatesPath)

	// 设置web菜单模板目录, 解析命令行参数web.console.libraries到cfg.web.ConsoleLibrariesPath, 默认console_libraries
	a.Flag("web.console.libraries", "Path to the console library directory.").
		Default("console_libraries").StringVar(&cfg.web.ConsoleLibrariesPath)

	// 设置web页面title信息, 解析命令行参数web.page-title到cfg.web.PageTitle, 默认Prometheus Time Series Collection and Processing Server
	a.Flag("web.page-title", "Document title of Prometheus instance.").
		Default("Prometheus Time Series Collection and Processing Server").StringVar(&cfg.web.PageTitle)

	// 设置prometheus跨域请求允许来源, 解析命令行参数web.cors.origin到cfg.corsRegexString, 默认.*
	a.Flag("web.cors.origin", `Regex for CORS origin. It is fully anchored. Example: 'https?://(domain1|domain2)\.com'`).
		Default(".*").StringVar(&cfg.corsRegexString)

	// 设置tsdb数据存储目录, 解析命令行参数storage.tsdb.path到cfg.localStoragePath, 默认data/
	a.Flag("storage.tsdb.path", "Base path for metrics storage.").
		Default("data/").StringVar(&cfg.localStoragePath)

	// TODO: prometheus source, 含义&用途, 解析命令行参数storage.tsdb.min-block-duration到cfg.tsdb.MinBlockDuration, 默认2h
	a.Flag("storage.tsdb.min-block-duration", "Minimum duration of a data block before being persisted. For use in testing.").
		Hidden().Default("2h").SetValue(&cfg.tsdb.MinBlockDuration)

	// TODO: prometheus source, 含义&用途, 解析命令行参数storage.tsdb.max-block-duration到cfg.tsdb.MaxBlockDuration
	a.Flag("storage.tsdb.max-block-duration",
		"Maximum duration compacted blocks may span. For use in testing. (Defaults to 10% of the retention period.)").
		Hidden().PlaceHolder("<duration>").SetValue(&cfg.tsdb.MaxBlockDuration)

	// 设置tsdb wal文件大小, 解析命令行参数storage.tsdb.wal-segment-size到cfg.tsdb.WALSegmentSize, 若设置必须在10MB and 256MB
	a.Flag("storage.tsdb.wal-segment-size",
		"Size at which to split the tsdb WAL segment files. Example: 100MB").
		Hidden().PlaceHolder("<bytes>").BytesVar(&cfg.tsdb.WALSegmentSize)

	// tsdb保留数据时间, 解析命令行参数storage.tsdb.retention到oldFlagRetentionDuration
	a.Flag("storage.tsdb.retention", "[DEPRECATED] How long to retain samples in storage. This flag has been deprecated, use \"storage.tsdb.retention.time\" instead.").
		SetValue(&oldFlagRetentionDuration)

	//  tsdb保留数据时间, 解析命令行参数storage.tsdb.retention.time到newFlagRetentionDuration
	a.Flag("storage.tsdb.retention.time", "How long to retain samples in storage. When this flag is set it overrides \"storage.tsdb.retention\". If neither this flag nor \"storage.tsdb.retention\" nor \"storage.tsdb.retention.size\" is set, the retention time defaults to "+defaultRetentionString+". Units Supported: y, w, d, h, m, s, ms.").
		SetValue(&newFlagRetentionDuration)

	// tsdb保留数据大小, 解析命令行参数storage.tsdb.retention.size到cfg.tsdb.MaxBytes
	a.Flag("storage.tsdb.retention.size", "[EXPERIMENTAL] Maximum number of bytes that can be stored for blocks. Units supported: KB, MB, GB, TB, PB. This flag is experimental and can be changed in future releases.").
		BytesVar(&cfg.tsdb.MaxBytes)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.tsdb.no-lockfile到cfg.tsdb.NoLockfile, 默认false
	a.Flag("storage.tsdb.no-lockfile", "Do not create lockfile in data directory.").
		Default("false").BoolVar(&cfg.tsdb.NoLockfile)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.tsdb.allow-overlapping-blocks到cfg.tsdb.AllowOverlappingBlocks, 默认为false
	a.Flag("storage.tsdb.allow-overlapping-blocks", "[EXPERIMENTAL] Allow overlapping blocks, which in turn enables vertical compaction and vertical query merge.").
		Default("false").BoolVar(&cfg.tsdb.AllowOverlappingBlocks)

	// 设置是否启用对tsdb wal文件压缩, 解析命令行参数storage.tsdb.wal-compression到cfg.tsdb.WALCompression, 默认false
	a.Flag("storage.tsdb.wal-compression", "Compress the tsdb WAL.").
		Default("false").BoolVar(&cfg.tsdb.WALCompression)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.remote.flush-deadline到cfg.RemoteFlushDeadline, 默认1m
	a.Flag("storage.remote.flush-deadline", "How long to wait flushing sample on shutdown or config reload.").
		Default("1m").PlaceHolder("<duration>").SetValue(&cfg.RemoteFlushDeadline)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.remote.read-sample-limit到cfg.web.RemoteReadSampleLimit, 默认5e7
	a.Flag("storage.remote.read-sample-limit", "Maximum overall number of samples to return via the remote read interface, in a single query. 0 means no limit. This limit is ignored for streamed response types.").
		Default("5e7").IntVar(&cfg.web.RemoteReadSampleLimit)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.remote.read-concurrent-limit到cfg.web.RemoteReadConcurrencyLimit, 默认5e7
	a.Flag("storage.remote.read-concurrent-limit", "Maximum number of concurrent remote read calls. 0 means no limit.").
		Default("10").IntVar(&cfg.web.RemoteReadConcurrencyLimit)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数storage.remote.read-max-bytes-in-frame到cfg.web.RemoteReadBytesInFrame, 默认1048576
	a.Flag("storage.remote.read-max-bytes-in-frame", "Maximum number of bytes in a single frame for streaming remote read response types before marshalling. Note that client might have limit on frame size as well. 1MB as recommended by protobuf by default.").
		Default("1048576").IntVar(&cfg.web.RemoteReadBytesInFrame)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数rules.alert.for-outage-tolerance到cfg.outageTolerance, 默认1h
	a.Flag("rules.alert.for-outage-tolerance", "Max time to tolerate prometheus outage for restoring \"for\" state of alert.").
		Default("1h").SetValue(&cfg.outageTolerance)

	// 告警恢复时间, 解析命令行参数rules.alert.for-grace-period到cfg.forGracePeriod, 默认10m
	a.Flag("rules.alert.for-grace-period", "Minimum duration between alert and restored \"for\" state. This is maintained only for alerts with configured \"for\" time greater than grace period.").
		Default("10m").SetValue(&cfg.forGracePeriod)

	// 告警静默时间, 解析命令行参数rules.alert.resend-delay到cfg.resendDelay, 默认1m
	a.Flag("rules.alert.resend-delay", "Minimum amount of time to wait before resending an alert to Alertmanager.").
		Default("1m").SetValue(&cfg.resendDelay)

	// 告警通知管道长度, 解析命令行参数rules.alert.resend-delay到cfg.notifier.QueueCapacity, 默认10000
	a.Flag("alertmanager.notification-queue-capacity", "The capacity of the queue for pending Alertmanager notifications.").
		Default("10000").IntVar(&cfg.notifier.QueueCapacity)

	// [未使用]通知超时时间, 解析命令行参数alertmanager.timeout到cfg.notifierTimeout, 默认10s
	a.Flag("alertmanager.timeout", "Timeout for sending alerts to Alertmanager.").
		Default("10s").SetValue(&cfg.notifierTimeout)

	// TODO: prometheus source, 参数含义&用途, 解析命令行参数query.lookback-delta到cfg.lookbackDelta, 默认5m
	a.Flag("query.lookback-delta", "The maximum lookback duration for retrieving metrics during expression evaluations.").
		Default("5m").SetValue(&cfg.lookbackDelta)

	// TODO: prometheus source, 确认, promql查询超时时间, 解析命令行参数query.timeout到cfg.queryTimeout, 默认2m
	a.Flag("query.timeout", "Maximum time a query may take before being aborted.").
		Default("2m").SetValue(&cfg.queryTimeout)

	// TODO: prometheus source, 确认, promql查询并发限制, 解析命令行参数query.max-concurrency到cfg.queryConcurrency, 默认20
	a.Flag("query.max-concurrency", "Maximum number of queries executed concurrently.").
		Default("20").IntVar(&cfg.queryConcurrency)

	// TODO: prometheus source, 确认, promql查询数据限制, 解析命令行参数query.max-samples到cfg.queryMaxSamples, 默认50000000
	a.Flag("query.max-samples", "Maximum number of samples a single query can load into memory. Note that queries will fail if they try to load more samples than this into memory, so this also limits the number of samples a query can return.").
		Default("50000000").IntVar(&cfg.queryMaxSamples)

	// 设置记录日志格式和级别, 解析命令行参数log.level到cfg.promlogConfig.Level(debug, info, warn, error), 默认info. 解析命令行参数log.format到cfg.promlogConfig.Format(logfmt, json), 默认logfmt
	promlogflag.AddFlags(a, &cfg.promlogConfig)

	// 解析命令行参数
	_, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "Error parsing commandline arguments"))
		a.Usage(os.Args[1:])
		os.Exit(2)
	}

	// 创建日志记录器
	logger := promlog.New(&cfg.promlogConfig)

	// 处理prometheus外部访问url
	cfg.web.ExternalURL, err = computeExternalURL(cfg.prometheusURL, cfg.web.ListenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "parse external URL %q", cfg.prometheusURL))
		os.Exit(2)
	}

	// 处理prometheus跨域请求来源正则表达式
	cfg.web.CORSOrigin, err = compileCORSRegexString(cfg.corsRegexString)
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Wrapf(err, "could not compile CORS regex string %q", cfg.corsRegexString))
		os.Exit(2)
	}

	// 处理web读请求超时时间
	cfg.web.ReadTimeout = time.Duration(cfg.webTimeout)

	// Default -web.route-prefix to path of -web.external-url.
	// 处理web请求url前缀
	if cfg.web.RoutePrefix == "" {
		cfg.web.RoutePrefix = cfg.web.ExternalURL.Path
	}
	// RoutePrefix must always be at least '/'.
	// 格式化url前缀
	cfg.web.RoutePrefix = "/" + strings.Trim(cfg.web.RoutePrefix, "/")

	{ // Time retention settings.
		// 时序数据库配置, 数据保留时间
		if oldFlagRetentionDuration != 0 {
			level.Warn(logger).Log("deprecation_notice", "'storage.tsdb.retention' flag is deprecated use 'storage.tsdb.retention.time' instead.")
			cfg.tsdb.RetentionDuration = oldFlagRetentionDuration
		}

		// When the new flag is set it takes precedence.
		if newFlagRetentionDuration != 0 {
			cfg.tsdb.RetentionDuration = newFlagRetentionDuration
		}

		// 未配置时保留15d
		if cfg.tsdb.RetentionDuration == 0 && cfg.tsdb.MaxBytes == 0 {
			cfg.tsdb.RetentionDuration = defaultRetentionDuration
			level.Info(logger).Log("msg", "no time or size retention was set so using the default time retention", "duration", defaultRetentionDuration)
		}

		// Check for overflows. This limits our max retention to 100y.
		// 若保留时间<0, 则保留100y
		if cfg.tsdb.RetentionDuration < 0 {
			y, err := model.ParseDuration("100y")
			if err != nil {
				panic(err)
			}
			cfg.tsdb.RetentionDuration = y
			level.Warn(logger).Log("msg", "time retention value is too high. Limiting to: "+y.String())
		}
	}

	{ // Max block size  settings.
		// TODO: 时序数据库配置, 含义和用途
		if cfg.tsdb.MaxBlockDuration == 0 {
			maxBlockDuration, err := model.ParseDuration("31d")
			if err != nil {
				panic(err)
			}
			// When the time retention is set and not too big use to define the max block duration.
			if cfg.tsdb.RetentionDuration != 0 && cfg.tsdb.RetentionDuration/10 < maxBlockDuration {
				maxBlockDuration = cfg.tsdb.RetentionDuration / 10
			}

			cfg.tsdb.MaxBlockDuration = maxBlockDuration
		}
	}

	// TODO: promql配置, 含义和用途
	promql.LookbackDelta = time.Duration(cfg.lookbackDelta)
	promql.SetDefaultEvaluationInterval(time.Duration(config.DefaultGlobalConfig.EvaluationInterval))

	// Above level 6, the k8s client would log bearer tokens in clear-text.
	// TODO: k8s日志配置, 用途
	klog.ClampLevel(6)
	klog.SetLogger(log.With(logger, "component", "k8s_client_runtime"))

	level.Info(logger).Log("msg", "Starting Prometheus", "version", version.Info())
	level.Info(logger).Log("build_context", version.BuildContext())
	level.Info(logger).Log("host_details", prom_runtime.Uname())
	level.Info(logger).Log("fd_limits", prom_runtime.FdLimits())
	level.Info(logger).Log("vm_limits", prom_runtime.VmLimits())

	var (
		// 定义本地存储器
		localStorage = &tsdb.ReadyStorage{}

		// 定义远程存储器
		remoteStorage = remote.NewStorage(log.With(logger, "component", "remote"), prometheus.DefaultRegisterer, localStorage.StartTime, cfg.localStoragePath, time.Duration(cfg.RemoteFlushDeadline))

		// 定义扇出存储器, local为主, remote为从
		fanoutStorage = storage.NewFanout(logger, localStorage, remoteStorage)
	)

	var (
		// web上下文
		ctxWeb, cancelWeb = context.WithCancel(context.Background())

		// 规则上下文
		ctxRule = context.Background()

		// 通知器实例
		notifierManager = notifier.NewManager(&cfg.notifier, log.With(logger, "component", "notifier"))

		// 指标信息采集上下文
		ctxScrape, cancelScrape = context.WithCancel(context.Background())

		// 采集配置管理实例, 用于监控采集配置变更后通知采集器
		discoveryManagerScrape = discovery.NewManager(ctxScrape, log.With(logger, "component", "discovery manager scrape"), discovery.Name("scrape"))

		// 通知上下文
		ctxNotify, cancelNotify = context.WithCancel(context.Background())

		// 告警通知规则配置管理实例, 用于监控告警通知规则配置变更后通知器
		discoveryManagerNotify = discovery.NewManager(ctxNotify, log.With(logger, "component", "discovery manager notify"), discovery.Name("notify"))

		// 采集器实例
		scrapeManager = scrape.NewManager(log.With(logger, "component", "scrape manager"), fanoutStorage)

		// 查询引擎配置
		opts = promql.EngineOpts{
			Logger:             log.With(logger, "component", "query engine"),
			Reg:                prometheus.DefaultRegisterer,
			MaxSamples:         cfg.queryMaxSamples,
			Timeout:            time.Duration(cfg.queryTimeout),
			ActiveQueryTracker: promql.NewActiveQueryTracker(cfg.localStoragePath, cfg.queryConcurrency, log.With(logger, "component", "activeQueryTracker")),
		}

		// 查询引擎实例
		queryEngine = promql.NewEngine(opts)

		// 规则引擎实例, 用于产生告警
		ruleManager = rules.NewManager(&rules.ManagerOptions{
			Appendable:      fanoutStorage,
			TSDB:            localStorage,
			QueryFunc:       rules.EngineQueryFunc(queryEngine, fanoutStorage),
			NotifyFunc:      sendAlerts(notifierManager, cfg.web.ExternalURL.String()),
			Context:         ctxRule,
			ExternalURL:     cfg.web.ExternalURL,
			Registerer:      prometheus.DefaultRegisterer,
			Logger:          log.With(logger, "component", "rule manager"),
			OutageTolerance: time.Duration(cfg.outageTolerance),
			ForGracePeriod:  time.Duration(cfg.forGracePeriod),
			ResendDelay:     time.Duration(cfg.resendDelay),
		})
	)

	// 设置web配置信息
	cfg.web.Context = ctxWeb              // web上下文
	cfg.web.TSDB = localStorage.Get       // 本地tsdb数据库连接
	cfg.web.Storage = fanoutStorage       // 扇出存储器
	cfg.web.QueryEngine = queryEngine     // 查询引擎
	cfg.web.ScrapeManager = scrapeManager // 采集器实例
	cfg.web.RuleManager = ruleManager     // 规则引擎实例
	cfg.web.Notifier = notifierManager    // 通知器实例
	cfg.web.TSDBCfg = cfg.tsdb            // tsdb配置信息

	// prometheus版本信息
	cfg.web.Version = &web.PrometheusVersion{
		Version:   version.Version,
		Revision:  version.Revision,
		Branch:    version.Branch,
		BuildUser: version.BuildUser,
		BuildDate: version.BuildDate,
		GoVersion: version.GoVersion,
	}

	// 设置命令行参数
	cfg.web.Flags = map[string]string{}

	// Exclude kingpin default flags to expose only Prometheus ones.
	// 只保留prometheus命令行参数(移除掉kingpin自有参数)
	boilerplateFlags := kingpin.New("", "").Version("")
	for _, f := range a.Model().Flags {
		if boilerplateFlags.GetFlag(f.Name) != nil {
			continue
		}

		cfg.web.Flags[f.Name] = f.Value.String()
	}

	// Depends on cfg.web.ScrapeManager so needs to be after cfg.web.ScrapeManager = scrapeManager.
	// 定义web处理器实例
	webHandler := web.New(log.With(logger, "component", "web"), &cfg.web)

	// Monitor outgoing connections on default transport with conntrack.
	/*
		Transport: 用于多个连接的状态信息, 缓存tcp连接, 可进行重用
		DialContext: 当新建连接时进行调用
	*/
	// 使用conntrack跟踪网络连接, 内置prometheus指标项统计
	http.DefaultTransport.(*http.Transport).DialContext = conntrack.NewDialContextFunc(
		conntrack.DialWithTracing(),
	)

	// 对各组件重新加载配置
	reloaders := []func(cfg *config.Config) error{
		remoteStorage.ApplyConfig, // 远程存储应用配置
		webHandler.ApplyConfig,    // web处理器应用配置
		func(cfg *config.Config) error { // 查询引擎引用配置, 设置查询日志记录器
			if cfg.GlobalConfig.QueryLogFile == "" {
				queryEngine.SetQueryLogger(nil)
				return nil
			}

			l, err := logging.NewJSONFileLogger(cfg.GlobalConfig.QueryLogFile)
			if err != nil {
				return err
			}
			queryEngine.SetQueryLogger(l)
			return nil
		},
		// The Scrape and notifier managers need to reload before the Discovery manager as
		// they need to read the most updated config when receiving the new targets list.
		scrapeManager.ApplyConfig, // 采集器应用配置
		func(cfg *config.Config) error { // 采集配置管理应用配置
			c := make(map[string]sd_config.ServiceDiscoveryConfig)

			// 获取job的采集配置信息
			for _, v := range cfg.ScrapeConfigs {
				c[v.JobName] = v.ServiceDiscoveryConfig
			}

			// 应用配置
			// TODO ApplyConfig内部实现
			return discoveryManagerScrape.ApplyConfig(c)
		},
		notifierManager.ApplyConfig, // 通知器应用配置
		func(cfg *config.Config) error { // 告警通知规则配置管理应用配置
			c := make(map[string]sd_config.ServiceDiscoveryConfig)

			// 获取告警管理配置信息
			for k, v := range cfg.AlertingConfig.AlertmanagerConfigs.ToMap() {
				c[k] = v.ServiceDiscoveryConfig
			}

			// 应用配置
			// TODO ApplyConfig内部实现
			return discoveryManagerNotify.ApplyConfig(c)
		},
		func(cfg *config.Config) error { // 规则引擎应用配置(规则文件)
			// Get all rule files matching the configuration paths.
			var files []string
			// 获取告警规则文件glob配置
			for _, pat := range cfg.RuleFiles {
				// 获取告警规则文件
				fs, err := filepath.Glob(pat)
				if err != nil {
					// The only error can be a bad pattern.
					return errors.Wrapf(err, "error retrieving rule files for %s", pat)
				}
				files = append(files, fs...)
			}

			// 更新配置
			// TODO Update内部实现
			return ruleManager.Update(
				time.Duration(cfg.GlobalConfig.EvaluationInterval),
				files,
				cfg.GlobalConfig.ExternalLabels,
			)
		},
	}

	// 注册配置成功指标信息
	prometheus.MustRegister(configSuccess)

	// 注册配置成功时间指标信息
	prometheus.MustRegister(configSuccessTime)

	// Start all components while we wait for TSDB to open but only load
	// initial config and mark ourselves as ready after it completed.
	// 定义管道, 用于控制tsdb数据库初始化完成后进行配置加载
	dbOpen := make(chan struct{})

	// sync.Once is used to make sure we can close the channel at different execution stages(SIGTERM or when the config is loaded).
	type closeOnce struct {
		C     chan struct{}
		once  sync.Once
		Close func()
	}
	// Wait until the server is ready to handle reloading.
	// 定义管道, 用于控制配置初始化完成后启动采集器, reload, 规则引擎, 通知器
	reloadReady := &closeOnce{
		C: make(chan struct{}),
	}

	// 关闭管道, 通过once修饰最多执行一次
	reloadReady.Close = func() {
		reloadReady.once.Do(func() {
			close(reloadReady.C)
		})
	}

	// 定义例程组
	var g run.Group
	{
		// Termination handler.
		// 监听终端ctrl+c和kill信号, 或web退出信号, 结束例程组
		term := make(chan os.Signal, 1)
		signal.Notify(term, os.Interrupt, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				// Don't forget to release the reloadReady channel so that waiting blocks can exit normally.
				select {
				case <-term:
					level.Warn(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
					reloadReady.Close()
				case <-webHandler.Quit():
					level.Warn(logger).Log("msg", "Received termination request via web service, exiting gracefully...")
				case <-cancel:
					reloadReady.Close()
				}
				return nil
			},
			func(err error) {
				close(cancel) // 关闭管道, 停止例程
			},
		)
	}
	{
		// Scrape discovery manager.
		// 启动采集配置管理实例, 用于格式化targetGroup, 并写入管道等待采集器更新
		g.Add(
			func() error {
				err := discoveryManagerScrape.Run()
				level.Info(logger).Log("msg", "Scrape discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping scrape discovery manager...")
				cancelScrape() // 停止采集器配置管道处理例程
			},
		)
	}
	{
		// Notify discovery manager.
		// 启动告警通知规则配置管理实例, 用于格式化targetGroup, 并写入管道等待通知器更新
		g.Add(
			func() error {
				err := discoveryManagerNotify.Run()
				level.Info(logger).Log("msg", "Notify discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping notify discovery manager...")
				cancelNotify() // 停止告警通知规则配置管道处理例程
			},
		)
	}
	{
		// Scrape manager.
		// 启动采集器, 通过采集配置管理实例管道更新采集池
		g.Add(
			func() error {
				// When the scrape manager receives a new targets list
				// it needs to read a valid config for each job.
				// It depends on the config being in sync with the discovery manager so
				// we wait until the config is fully loaded.
				<-reloadReady.C // 待配置初始化后执行

				err := scrapeManager.Run(discoveryManagerScrape.SyncCh())
				level.Info(logger).Log("msg", "Scrape manager stopped")
				return err
			},
			func(err error) {
				// Scrape manager needs to be stopped before closing the local TSDB
				// so that it doesn't try to write samples to a closed storage.
				level.Info(logger).Log("msg", "Stopping scrape manager...")
				scrapeManager.Stop() // 停止采集器
			},
		)
	}
	{
		// Reload handler.
		// reload处理器, 监听kill -1信号和web重载信号

		// Make sure that sighup handler is registered with a redirect to the channel before the potentially
		// long and synchronous tsdb init.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				<-reloadReady.C // 待配置初始化后执行

				for {
					select {
					case <-hup:
						if err := reloadConfig(cfg.configFile, logger, reloaders...); err != nil { //kill -1触发加载配置并更新各组件配置
							level.Error(logger).Log("msg", "Error reloading config", "err", err)
						}
					case rc := <-webHandler.Reload():
						if err := reloadConfig(cfg.configFile, logger, reloaders...); err != nil { //web reload api触发加载配置并更新各组件配置
							level.Error(logger).Log("msg", "Error reloading config", "err", err)
							rc <- err
						} else {
							rc <- nil
						}
					case <-cancel:
						return nil
					}
				}

			},
			func(err error) {
				// Wait for any in-progress reloads to complete to avoid
				// reloading things after they have been shutdown.
				cancel <- struct{}{} // 关闭管道, 停止例程
			},
		)
	}
	{
		// Initial configuration loading.
		// 在数据库链接完成后, 初始化配置
		cancel := make(chan struct{})
		g.Add(
			func() error {
				select {
				case <-dbOpen:
					break
				// In case a shutdown is initiated before the dbOpen is released
				case <-cancel:
					reloadReady.Close()
					return nil
				}

				// 加载配置, 并对各组件进行调用
				if err := reloadConfig(cfg.configFile, logger, reloaders...); err != nil {
					return errors.Wrapf(err, "error loading config from %q", cfg.configFile)
				}

				// 配置加载完成, 通知采集器, reload, 规则引擎, 通知器例程进行启动
				reloadReady.Close()

				// 设置web处理器ready状态
				webHandler.Ready()
				level.Info(logger).Log("msg", "Server is ready to receive web requests.")
				<-cancel
				return nil
			},
			func(err error) {
				close(cancel) // 关闭管道, 停止例程
			},
		)
	}
	{
		// Rule manager.
		// 启动规则引擎
		// TODO(krasi) refactor ruleManager.Run() to be blocking to avoid using an extra blocking channel.
		cancel := make(chan struct{})
		g.Add(
			func() error {
				<-reloadReady.C   // 待配置初始化后执行
				ruleManager.Run() // 启动规则引擎
				<-cancel
				return nil
			},
			func(err error) {
				ruleManager.Stop() // 停止规则引擎
				close(cancel)      // 关闭管道, 停止例程
			},
		)
	}
	{
		// TSDB.
		// 启动TSDB
		cancel := make(chan struct{})
		g.Add(
			func() error {
				level.Info(logger).Log("msg", "Starting TSDB ...")
				if cfg.tsdb.WALSegmentSize != 0 {
					if cfg.tsdb.WALSegmentSize < 10*1024*1024 || cfg.tsdb.WALSegmentSize > 256*1024*1024 {
						return errors.New("flag 'storage.tsdb.wal-segment-size' must be set between 10MB and 256MB")
					}
				}
				// 打开TSDB连接接
				db, err := tsdb.Open(
					cfg.localStoragePath,
					log.With(logger, "component", "tsdb"),
					prometheus.DefaultRegisterer,
					&cfg.tsdb,
				)
				if err != nil {
					return errors.Wrapf(err, "opening storage failed")
				}
				level.Info(logger).Log("fs_type", prom_runtime.Statfs(cfg.localStoragePath))
				level.Info(logger).Log("msg", "TSDB started")
				level.Debug(logger).Log("msg", "TSDB options",
					"MinBlockDuration", cfg.tsdb.MinBlockDuration,
					"MaxBlockDuration", cfg.tsdb.MaxBlockDuration,
					"MaxBytes", cfg.tsdb.MaxBytes,
					"NoLockfile", cfg.tsdb.NoLockfile,
					"RetentionDuration", cfg.tsdb.RetentionDuration,
					"WALSegmentSize", cfg.tsdb.WALSegmentSize,
					"AllowOverlappingBlocks", cfg.tsdb.AllowOverlappingBlocks,
					"WALCompression", cfg.tsdb.WALCompression,
				)

				// TODO: prometheus source, 用途
				startTimeMargin := int64(2 * time.Duration(cfg.tsdb.MinBlockDuration).Seconds() * 1000)

				// 设置本地存储TSDB连接
				localStorage.Set(db, startTimeMargin)

				// 关闭管道, 通知初始化配置例程
				close(dbOpen)
				<-cancel
				return nil
			},
			func(err error) {
				if err := fanoutStorage.Close(); err != nil { // 关闭扇出存储器
					level.Error(logger).Log("msg", "Error stopping storage", "err", err)
				}
				close(cancel)
			},
		)
	}
	{
		// Web handler.
		// 启动web处理器
		g.Add(
			func() error {
				if err := webHandler.Run(ctxWeb); err != nil { // 启动web处理器
					return errors.Wrapf(err, "error starting web server")
				}
				return nil
			},
			func(err error) {
				cancelWeb() // 停止web处理器
			},
		)
	}
	{
		// Notifier.
		// 启动通知器, 通过告警通知规则配置管理实例管道更新alartmanager
		// Calling notifier.Stop() before ruleManager.Stop() will cause a panic if the ruleManager isn't running,
		// so keep this interrupt after the ruleManager.Stop().
		g.Add(
			func() error {
				// When the notifier manager receives a new targets list
				// it needs to read a valid config for each job.
				// It depends on the config being in sync with the discovery manager
				// so we wait until the config is fully loaded.
				<-reloadReady.C // 待配置初始化后执行

				notifierManager.Run(discoveryManagerNotify.SyncCh()) // 启动通知器
				level.Info(logger).Log("msg", "Notifier manager stopped")
				return nil
			},
			func(err error) {
				notifierManager.Stop() // 停止通知器
			},
		)
	}

	// 启动例程组
	if err := g.Run(); err != nil {
		level.Error(logger).Log("err", err)
		os.Exit(1)
	}
	level.Info(logger).Log("msg", "See you next time!")
}

func reloadConfig(filename string, logger log.Logger, rls ...func(*config.Config) error) (err error) {
	// 加载配置并调用各组件进行加载
	level.Info(logger).Log("msg", "Loading configuration file", "filename", filename)

	// 设置配置成功,时间指标信息
	defer func() {
		if err == nil {
			configSuccess.Set(1)
			configSuccessTime.SetToCurrentTime()
		} else {
			configSuccess.Set(0)
		}
	}()

	// 反序列化配置文件内容到结构体
	conf, err := config.LoadFile(filename)
	if err != nil {
		return errors.Wrapf(err, "couldn't load configuration (--config.file=%q)", filename)
	}

	failed := false
	// 调用各组件加载配置
	for _, rl := range rls {
		if err := rl(conf); err != nil {
			level.Error(logger).Log("msg", "Failed to apply configuration", "err", err)
			failed = true
		}
	}
	if failed {
		return errors.Errorf("one or more errors occurred while applying the new configuration (--config.file=%q)", filename)
	}

	// TODO: prometheus source, 用途和含义
	promql.SetDefaultEvaluationInterval(time.Duration(conf.GlobalConfig.EvaluationInterval))
	level.Info(logger).Log("msg", "Completed loading of configuration file", "filename", filename)
	return nil
}

func startsOrEndsWithQuote(s string) bool {
	return strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") ||
		strings.HasSuffix(s, "\"") || strings.HasSuffix(s, "'")
}

// compileCORSRegexString compiles given string and adds anchors
func compileCORSRegexString(s string) (*regexp.Regexp, error) {
	// 创建跨域请求来源正则表达式
	r, err := relabel.NewRegexp(s)
	if err != nil {
		return nil, err
	}
	return r.Regexp, nil
}

// computeExternalURL computes a sanitized external URL from a raw input. It infers unset
// URL parts from the OS and the given listen address.
func computeExternalURL(u, listenAddr string) (*url.URL, error) {
	// 格式化外部访问URL

	// 命令行未指定web.external-url, 则根据当前主机信息和监听端口生成请求url
	if u == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return nil, err
		}
		u = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	// 检查url是否以'或"开头或结尾
	if startsOrEndsWithQuote(u) {
		return nil, errors.New("URL must not begin or end with quotes")
	}

	// 解析url
	eu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	// 对path进行格式化处理, 去除掉url结尾的/
	ppref := strings.TrimRight(eu.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	eu.Path = ppref

	return eu, nil
}

// 发送器接口
type sender interface {
	Send(alerts ...*notifier.Alert)
}

// sendAlerts implements the rules.NotifyFunc for a Notifier.
func sendAlerts(s sender, externalURL string) rules.NotifyFunc {
	// 获取告警发送器
	return func(ctx context.Context, expr string, alerts ...*rules.Alert) {
		var res []*notifier.Alert

		for _, alert := range alerts {
			// 定义告警通知信息
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt, // 发生时间
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt // 恢复
			} else {
				a.EndsAt = alert.ValidUntil // 故障
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			s.Send(res...) // 发送告警
		}
	}
}
