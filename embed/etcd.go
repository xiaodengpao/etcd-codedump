// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package embed

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	defaultLog "log"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"go.etcd.io/etcd/etcdserver"
	"go.etcd.io/etcd/etcdserver/api/etcdhttp"
	"go.etcd.io/etcd/etcdserver/api/rafthttp"
	"go.etcd.io/etcd/etcdserver/api/v2http"
	"go.etcd.io/etcd/etcdserver/api/v2v3"
	"go.etcd.io/etcd/etcdserver/api/v3client"
	"go.etcd.io/etcd/etcdserver/api/v3rpc"
	"go.etcd.io/etcd/pkg/debugutil"
	runtimeutil "go.etcd.io/etcd/pkg/runtime"
	"go.etcd.io/etcd/pkg/transport"
	"go.etcd.io/etcd/pkg/types"
	"go.etcd.io/etcd/version"

	"github.com/coreos/pkg/capnslog"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/soheilhy/cmux"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

var plog = capnslog.NewPackageLogger("go.etcd.io/etcd", "embed")

const (
	// internal fd usage includes disk usage and transport usage.
	// To read/write snapshot, snap pkg needs 1. In normal case, wal pkg needs
	// at most 2 to read/lock/write WALs. One case that it needs to 2 is to
	// read all logs after some snapshot index, which locates at the end of
	// the second last and the head of the last. For purging, it needs to read
	// directory, so it needs 1. For fd monitor, it needs 1.
	// For transport, rafthttp builds two long-polling connections and at most
	// four temporary connections with each member. There are at most 9 members
	// in a cluster, so it should reserve 96.
	// For the safety, we set the total reserved number to 150.
	reservedInternalFDNum = 150
)

// Etcd contains a running etcd server and its listeners.
// Etcd结构体
type Etcd struct {
	// 此Peers和raftNode中的peers不是一码事，是ETCD启动时参数指定的。
	// Listener的数组，一般一个就够用了（我觉得）
	Peers []*peerListener

	// 向etcd集群发送HTTP请求的客户端
	// 类似于Peers，Clients是netListener，监听client端的网络请求
	Clients []net.Listener

	sctxs            map[string]*serveCtx // 服务client的网络上下文Map
	metricsListeners []net.Listener       // metrics？

	Server *etcdserver.EtcdServer // Etcd核心服务

	cfg   Config        // 配置参数
	stopc chan struct{} // 服务停止监听，任何服务几乎都要 <-stopc，了解服务何时退出
	errc  chan error

	closeOnce sync.Once // 执行一次
}

type peerListener struct {
	net.Listener
	serve func() error
	close func(context.Context) error
}

// StartEtcd .
// |-embed.StartEtcd() —>           embed/etcd.go
// |  |-configurePeerListeners()     peer网络监听，服务于raft
// |  |-configureClientListeners()   client网络监听，服务于client
// |  |-EtcdServer.ServerConfig()    生成新的配置
// |  |
// |  |-EtcdServer.NewServer()       etcdserver/server.go 正式启动RAFT服务<<<1>>>
// |  |
// |  |-EtcdServer.Start() ->        etcdserver/server.go 开始启动服务
// |  | |-EtcdServer.start() ->
// |  |   |-wait.New()               新建WaitGroup组以及一些管道服务
// |  |
// |  |   |-EtcdServer.run()         etcdserver/raft.go 启动应用层的处理协程<<<2>>>
// |  |
// |  |-Etcd.servePeers()            启动集群内部通讯
// |  | 好的
// |  | |-v3rpc.Server()             启动gRPC服务 api/v3rpc/grpc.go，这里真正监听了frpc请求
// |  |   |-grpc.NewServer()         调用gRPC的接口创建
// |  |   |-pb.RegisterKVServer()    注册各种的服务，这里包含了多个
// |  |   |-pb.RegisterWatchServer()
// |  |
// |  |-Etcd.serveClients()          启动协程处理客户请求
func StartEtcd(inCfg *Config) (e *Etcd, err error) {
	fmt.Println("<===================>", "embd.StartEtcd \n")
	if err = inCfg.Validate(); err != nil {
		return nil, err
	}
	serving := false

	e = &Etcd{cfg: *inCfg, stopc: make(chan struct{})}
	cfg := &e.cfg
	defer func() {
		if e == nil || err == nil {
			return
		}
		if !serving {
			// errored before starting gRPC server for serveCtx.serversC
			for _, sctx := range e.sctxs {
				close(sctx.serversC)
			}
		}
		e.Close()
		e = nil
	}()

	if e.cfg.logger != nil {
		e.cfg.logger.Info(
			"configuring peer listeners",
			zap.Strings("listen-peer-urls", e.cfg.getLPURLs()),
		)
	}

	if e.Peers, err = configurePeerListeners(cfg); err != nil {
		return e, err
	}

	if e.cfg.logger != nil {
		e.cfg.logger.Info(
			"configuring client listeners",
			zap.Strings("listen-client-urls", e.cfg.getLCURLs()),
		)
	}
	if e.sctxs, err = configureClientListeners(cfg); err != nil {
		return e, err
	}

	for _, sctx := range e.sctxs {
		e.Clients = append(e.Clients, sctx.l)
	}

	var (
		urlsmap types.URLsMap
		token   string
	)
	memberInitialized := true
	if !isMemberInitialized(cfg) {
		memberInitialized = false
		urlsmap, token, err = cfg.PeerURLsMapAndToken("etcd")
		if err != nil {
			return e, fmt.Errorf("error setting up initial cluster: %v", err)
		}
	}

	if len(cfg.AutoCompactionRetention) == 0 {
		cfg.AutoCompactionRetention = "0"
	}
	autoCompactionRetention, err := parseCompactionRetention(cfg.AutoCompactionMode, cfg.AutoCompactionRetention)
	if err != nil {
		return e, err
	}

	backendFreelistType := parseBackendFreelistType(cfg.BackendFreelistType)

	srvcfg := etcdserver.ServerConfig{
		Name:                       cfg.Name,
		ClientURLs:                 cfg.ACUrls,
		PeerURLs:                   cfg.APUrls,
		DataDir:                    cfg.Dir,
		DedicatedWALDir:            cfg.WalDir,
		SnapshotCount:              cfg.SnapshotCount,
		SnapshotCatchUpEntries:     cfg.SnapshotCatchUpEntries,
		MaxSnapFiles:               cfg.MaxSnapFiles,
		MaxWALFiles:                cfg.MaxWalFiles,
		InitialPeerURLsMap:         urlsmap,
		InitialClusterToken:        token,
		DiscoveryURL:               cfg.Durl,
		DiscoveryProxy:             cfg.Dproxy,
		NewCluster:                 cfg.IsNewCluster(),
		PeerTLSInfo:                cfg.PeerTLSInfo,
		TickMs:                     cfg.TickMs,
		ElectionTicks:              cfg.ElectionTicks(),
		InitialElectionTickAdvance: cfg.InitialElectionTickAdvance,
		AutoCompactionRetention:    autoCompactionRetention,
		AutoCompactionMode:         cfg.AutoCompactionMode,
		QuotaBackendBytes:          cfg.QuotaBackendBytes,
		BackendBatchLimit:          cfg.BackendBatchLimit,
		BackendFreelistType:        backendFreelistType,
		BackendBatchInterval:       cfg.BackendBatchInterval,
		MaxTxnOps:                  cfg.MaxTxnOps,
		MaxRequestBytes:            cfg.MaxRequestBytes,
		StrictReconfigCheck:        cfg.StrictReconfigCheck,
		ClientCertAuthEnabled:      cfg.ClientTLSInfo.ClientCertAuth,
		AuthToken:                  cfg.AuthToken,
		BcryptCost:                 cfg.BcryptCost,
		CORS:                       cfg.CORS,
		HostWhitelist:              cfg.HostWhitelist,
		InitialCorruptCheck:        cfg.ExperimentalInitialCorruptCheck,
		CorruptCheckTime:           cfg.ExperimentalCorruptCheckTime,
		PreVote:                    cfg.PreVote,
		Logger:                     cfg.logger,
		LoggerConfig:               cfg.loggerConfig,
		LoggerCore:                 cfg.loggerCore,
		LoggerWriteSyncer:          cfg.loggerWriteSyncer,
		Debug:                      cfg.Debug,
		ForceNewCluster:            cfg.ForceNewCluster,
		EnableGRPCGateway:          cfg.EnableGRPCGateway,
		EnableLeaseCheckpoint:      cfg.ExperimentalEnableLeaseCheckpoint,
		CompactionBatchLimit:       cfg.ExperimentalCompactionBatchLimit,
	}
	print(e.cfg.logger, *cfg, srvcfg, memberInitialized)
	if e.Server, err = etcdserver.NewServer(srvcfg); err != nil {
		return e, err
	}

	// buffer channel so goroutines on closed connections won't wait forever
	e.errc = make(chan error, len(e.Peers)+len(e.Clients)+2*len(e.sctxs))

	// newly started member ("memberInitialized==false")
	// does not need corruption check
	if memberInitialized {
		if err = e.Server.CheckInitialHashKV(); err != nil {
			// set "EtcdServer" to nil, so that it does not block on "EtcdServer.Close()"
			// (nothing to close since rafthttp transports have not been started)
			e.Server = nil
			return e, err
		}
	}

	// 5. 启动核心服务
	e.Server.Start()

	// 6. 网络监听
	// 启动Etcd.Peer监听，等待集群中其他机器连接自己。
	if err = e.servePeers(); err != nil {
		return e, err
	}

	// 启动客户端监听Socket，等待客户端请求并响应
	if err = e.serveClients(); err != nil {
		return e, err
	}

	if err = e.serveMetrics(); err != nil {
		return e, err
	}

	if e.cfg.logger != nil {
		e.cfg.logger.Info(
			"now serving peer/client/metrics",
			zap.String("local-member-id", e.Server.ID().String()),
			zap.Strings("initial-advertise-peer-urls", e.cfg.getAPURLs()),
			zap.Strings("listen-peer-urls", e.cfg.getLPURLs()),
			zap.Strings("advertise-client-urls", e.cfg.getACURLs()),
			zap.Strings("listen-client-urls", e.cfg.getLCURLs()),
			zap.Strings("listen-metrics-urls", e.cfg.getMetricsURLs()),
		)
	}

	// 7. 标记位
	serving = true
	return e, nil
}

func print(lg *zap.Logger, ec Config, sc etcdserver.ServerConfig, memberInitialized bool) {
	// TODO: remove this after dropping "capnslog"
	if lg == nil {
		plog.Infof("name = %s", ec.Name)
		if sc.ForceNewCluster {
			plog.Infof("force new cluster")
		}
		plog.Infof("data dir = %s", sc.DataDir)
		plog.Infof("member dir = %s", sc.MemberDir())
		if sc.DedicatedWALDir != "" {
			plog.Infof("dedicated WAL dir = %s", sc.DedicatedWALDir)
		}
		plog.Infof("heartbeat = %dms", sc.TickMs)
		plog.Infof("election = %dms", sc.ElectionTicks*int(sc.TickMs))
		plog.Infof("snapshot count = %d", sc.SnapshotCount)
		if len(sc.DiscoveryURL) != 0 {
			plog.Infof("discovery URL= %s", sc.DiscoveryURL)
			if len(sc.DiscoveryProxy) != 0 {
				plog.Infof("discovery proxy = %s", sc.DiscoveryProxy)
			}
		}
		plog.Infof("advertise client URLs = %s", sc.ClientURLs)
		if memberInitialized {
			plog.Infof("initial advertise peer URLs = %s", sc.PeerURLs)
			plog.Infof("initial cluster = %s", sc.InitialPeerURLsMap)
		}
	} else {
		cors := make([]string, 0, len(ec.CORS))
		for v := range ec.CORS {
			cors = append(cors, v)
		}
		sort.Strings(cors)

		hss := make([]string, 0, len(ec.HostWhitelist))
		for v := range ec.HostWhitelist {
			hss = append(hss, v)
		}
		sort.Strings(hss)

		quota := ec.QuotaBackendBytes
		if quota == 0 {
			quota = etcdserver.DefaultQuotaBytes
		}

		lg.Info(
			"starting an etcd server",
			zap.String("etcd-version", version.Version),
			zap.String("git-sha", version.GitSHA),
			zap.String("go-version", runtime.Version()),
			zap.String("go-os", runtime.GOOS),
			zap.String("go-arch", runtime.GOARCH),
			zap.Int("max-cpu-set", runtime.GOMAXPROCS(0)),
			zap.Int("max-cpu-available", runtime.NumCPU()),
			zap.Bool("member-initialized", memberInitialized),
			zap.String("name", sc.Name),
			zap.String("data-dir", sc.DataDir),
			zap.String("wal-dir", ec.WalDir),
			zap.String("wal-dir-dedicated", sc.DedicatedWALDir),
			zap.String("member-dir", sc.MemberDir()),
			zap.Bool("force-new-cluster", sc.ForceNewCluster),
			zap.String("heartbeat-interval", fmt.Sprintf("%v", time.Duration(sc.TickMs)*time.Millisecond)),
			zap.String("election-timeout", fmt.Sprintf("%v", time.Duration(sc.ElectionTicks*int(sc.TickMs))*time.Millisecond)),
			zap.Bool("initial-election-tick-advance", sc.InitialElectionTickAdvance),
			zap.Uint64("snapshot-count", sc.SnapshotCount),
			zap.Uint64("snapshot-catchup-entries", sc.SnapshotCatchUpEntries),
			zap.Strings("initial-advertise-peer-urls", ec.getAPURLs()),
			zap.Strings("listen-peer-urls", ec.getLPURLs()),
			zap.Strings("advertise-client-urls", ec.getACURLs()),
			zap.Strings("listen-client-urls", ec.getLCURLs()),
			zap.Strings("listen-metrics-urls", ec.getMetricsURLs()),
			zap.Strings("cors", cors),
			zap.Strings("host-whitelist", hss),
			zap.String("initial-cluster", sc.InitialPeerURLsMap.String()),
			zap.String("initial-cluster-state", ec.ClusterState),
			zap.String("initial-cluster-token", sc.InitialClusterToken),
			zap.Int64("quota-size-bytes", quota),
			zap.Bool("pre-vote", sc.PreVote),
			zap.Bool("initial-corrupt-check", sc.InitialCorruptCheck),
			zap.String("corrupt-check-time-interval", sc.CorruptCheckTime.String()),
			zap.String("auto-compaction-mode", sc.AutoCompactionMode),
			zap.Duration("auto-compaction-retention", sc.AutoCompactionRetention),
			zap.String("auto-compaction-interval", sc.AutoCompactionRetention.String()),
			zap.String("discovery-url", sc.DiscoveryURL),
			zap.String("discovery-proxy", sc.DiscoveryProxy),
		)
	}
}

// Config returns the current configuration.
func (e *Etcd) Config() Config {
	return e.cfg
}

// Close gracefully shuts down all servers/listeners.
// Client requests will be terminated with request timeout.
// After timeout, enforce remaning requests be closed immediately.
func (e *Etcd) Close() {
	fields := []zap.Field{
		zap.String("name", e.cfg.Name),
		zap.String("data-dir", e.cfg.Dir),
		zap.Strings("advertise-peer-urls", e.cfg.getAPURLs()),
		zap.Strings("advertise-client-urls", e.cfg.getACURLs()),
	}
	lg := e.GetLogger()
	if lg != nil {
		lg.Info("closing etcd server", fields...)
	}
	defer func() {
		if lg != nil {
			lg.Info("closed etcd server", fields...)
			lg.Sync()
		}
	}()

	e.closeOnce.Do(func() { close(e.stopc) })

	// close client requests with request timeout
	timeout := 2 * time.Second
	if e.Server != nil {
		timeout = e.Server.Cfg.ReqTimeout()
	}
	for _, sctx := range e.sctxs {
		for ss := range sctx.serversC {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			stopServers(ctx, ss)
			cancel()
		}
	}

	for _, sctx := range e.sctxs {
		sctx.cancel()
	}

	for i := range e.Clients {
		if e.Clients[i] != nil {
			e.Clients[i].Close()
		}
	}

	for i := range e.metricsListeners {
		e.metricsListeners[i].Close()
	}

	// close rafthttp transports
	if e.Server != nil {
		e.Server.Stop()
	}

	// close all idle connections in peer handler (wait up to 1-second)
	for i := range e.Peers {
		if e.Peers[i] != nil && e.Peers[i].close != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			e.Peers[i].close(ctx)
			cancel()
		}
	}
}

func stopServers(ctx context.Context, ss *servers) {
	shutdownNow := func() {
		// first, close the http.Server
		ss.http.Shutdown(ctx)
		// then close grpc.Server; cancels all active RPCs
		ss.grpc.Stop()
	}

	// do not grpc.Server.GracefulStop with TLS enabled etcd server
	// See https://github.com/grpc/grpc-go/issues/1384#issuecomment-317124531
	// and https://github.com/etcd-io/etcd/issues/8916
	if ss.secure {
		shutdownNow()
		return
	}

	ch := make(chan struct{})
	go func() {
		defer close(ch)
		// close listeners to stop accepting new connections,
		// will block on any existing transports
		ss.grpc.GracefulStop()
	}()

	// wait until all pending RPCs are finished
	select {
	case <-ch:
	case <-ctx.Done():
		// took too long, manually close open transports
		// e.g. watch streams
		shutdownNow()

		// concurrent GracefulStop should be interrupted
		<-ch
	}
}

func (e *Etcd) Err() <-chan error { return e.errc }

func configurePeerListeners(cfg *Config) (peers []*peerListener, err error) {
	if err = updateCipherSuites(&cfg.PeerTLSInfo, cfg.CipherSuites); err != nil {
		return nil, err
	}
	if err = cfg.PeerSelfCert(); err != nil {
		if cfg.logger != nil {
			cfg.logger.Fatal("failed to get peer self-signed certs", zap.Error(err))
		} else {
			plog.Fatalf("could not get certs (%v)", err)
		}
	}
	if !cfg.PeerTLSInfo.Empty() {
		if cfg.logger != nil {
			cfg.logger.Info(
				"starting with peer TLS",
				zap.String("tls-info", fmt.Sprintf("%+v", cfg.PeerTLSInfo)),
				zap.Strings("cipher-suites", cfg.CipherSuites),
			)
		} else {
			plog.Infof("peerTLS: %s", cfg.PeerTLSInfo)
		}
	}
	peers = make([]*peerListener, len(cfg.LPUrls))
	defer func() {
		if err == nil {
			return
		}
		for i := range peers {
			if peers[i] != nil && peers[i].close != nil {
				if cfg.logger != nil {
					cfg.logger.Warn(
						"closing peer listener",
						zap.String("address", cfg.LPUrls[i].String()),
						zap.Error(err),
					)
				} else {
					plog.Info("stopping listening for peers on ", cfg.LPUrls[i].String())
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				peers[i].close(ctx)
				cancel()
			}
		}
	}()

	for i, u := range cfg.LPUrls {
		if u.Scheme == "http" {
			if !cfg.PeerTLSInfo.Empty() {
				if cfg.logger != nil {
					cfg.logger.Warn("scheme is HTTP while key and cert files are present; ignoring key and cert files", zap.String("peer-url", u.String()))
				} else {
					plog.Warningf("The scheme of peer url %s is HTTP while peer key/cert files are presented. Ignored peer key/cert files.", u.String())
				}
			}
			if cfg.PeerTLSInfo.ClientCertAuth {
				if cfg.logger != nil {
					cfg.logger.Warn("scheme is HTTP while --peer-client-cert-auth is enabled; ignoring client cert auth for this URL", zap.String("peer-url", u.String()))
				} else {
					plog.Warningf("The scheme of peer url %s is HTTP while client cert auth (--peer-client-cert-auth) is enabled. Ignored client cert auth for this url.", u.String())
				}
			}
		}
		peers[i] = &peerListener{close: func(context.Context) error { return nil }}
		peers[i].Listener, err = rafthttp.NewListener(u, &cfg.PeerTLSInfo)
		if err != nil {
			return nil, err
		}
		// once serve, overwrite with 'http.Server.Shutdown'
		peers[i].close = func(context.Context) error {
			return peers[i].Listener.Close()
		}
	}
	return peers, nil
}

// 启动网络监听
// 1. serve grpc
// 2. serve rafthttp + grpc
// 3. cmx是为了配合多网络监听而引入的一个库，文档可以看这里：https://github.com/soheilhy/cmux
func (e *Etcd) servePeers() (err error) {
	fmt.Println("<===================>", "embed.servePeers \n")

	// raft-http handler
	ph := etcdhttp.NewPeerHandler(e.GetLogger(), e.Server)
	var peerTLScfg *tls.Config
	if !e.cfg.PeerTLSInfo.Empty() {
		if peerTLScfg, err = e.cfg.PeerTLSInfo.ServerConfig(); err != nil {
			return err
		}
	}
	// 要创建的Listenner数，取决于ETCD配置，默认1
	// 不是etcd节点的peers
	for _, p := range e.Peers {
		u := p.Listener.Addr().String()
		gs := v3rpc.Server(e.Server, peerTLScfg)

		// Listener挂载到cmux下
		// m.Serve才会监听端口启动服务
		m := cmux.New(p.Listener)

		// http2请求，流量走这里
		// grpc请求优先走这个handler
		go gs.Serve(m.Match(cmux.HTTP2()))

		// HTTP.Server，根据协议的不同，转发不同的handler.ServeHttp
		srv := &http.Server{
			Handler:     grpcHandlerFunc(gs, ph),
			ReadTimeout: 5 * time.Minute,
			ErrorLog:    defaultLog.New(ioutil.Discard, "", 0), // do not log user error
		}

		// http请求走这里handler，适配V2版本client
		go srv.Serve(m.Match(cmux.Any()))

		// listerner的serve方法赋值
		// p.serve  ==  m.Serve
		p.serve = func() error { return m.Serve() }
		p.close = func(ctx context.Context) error {
			// gracefully shutdown http.Server
			// close open listeners, idle connections
			// until context cancel or time-out
			if e.cfg.logger != nil {
				e.cfg.logger.Info(
					"stopping serving peer traffic",
					zap.String("address", u),
				)
			}
			stopServers(ctx, &servers{secure: peerTLScfg != nil, grpc: gs, http: srv})
			if e.cfg.logger != nil {
				e.cfg.logger.Info(
					"stopped serving peer traffic",
					zap.String("address", u),
				)
			}
			return nil
		}
	}

	// start peer servers in a goroutine
	for _, pl := range e.Peers {
		go func(l *peerListener) {
			u := l.Addr().String()
			if e.cfg.logger != nil {
				e.cfg.logger.Info(
					"serving peer traffic",
					zap.String("address", u),
				)
			} else {
				plog.Info("listening for peers on ", u)
			}
			fmt.Println("<===================>", "raft peer 网络服务开始监听端口  peerListener.serve \n")
			// 监听listen-peer-urls, 和其他etcd节点进行通信, raft协议
			// l.serve == cmx.Serve
			// start serving
			e.errHandler(l.serve())
		}(pl)
	}
	return nil
}

func configureClientListeners(cfg *Config) (sctxs map[string]*serveCtx, err error) {
	if err = updateCipherSuites(&cfg.ClientTLSInfo, cfg.CipherSuites); err != nil {
		return nil, err
	}
	if err = cfg.ClientSelfCert(); err != nil {
		if cfg.logger != nil {
			cfg.logger.Fatal("failed to get client self-signed certs", zap.Error(err))
		} else {
			plog.Fatalf("could not get certs (%v)", err)
		}
	}
	if cfg.EnablePprof {
		if cfg.logger != nil {
			cfg.logger.Info("pprof is enabled", zap.String("path", debugutil.HTTPPrefixPProf))
		} else {
			plog.Infof("pprof is enabled under %s", debugutil.HTTPPrefixPProf)
		}
	}

	sctxs = make(map[string]*serveCtx)
	for _, u := range cfg.LCUrls {

		sctx := newServeCtx(cfg.logger)
		if u.Scheme == "http" || u.Scheme == "unix" {
			if !cfg.ClientTLSInfo.Empty() {
				if cfg.logger != nil {
					cfg.logger.Warn("scheme is HTTP while key and cert files are present; ignoring key and cert files", zap.String("client-url", u.String()))
				} else {
					plog.Warningf("The scheme of client url %s is HTTP while peer key/cert files are presented. Ignored key/cert files.", u.String())
				}
			}
			if cfg.ClientTLSInfo.ClientCertAuth {
				if cfg.logger != nil {
					cfg.logger.Warn("scheme is HTTP while --client-cert-auth is enabled; ignoring client cert auth for this URL", zap.String("client-url", u.String()))
				} else {
					plog.Warningf("The scheme of client url %s is HTTP while client cert auth (--client-cert-auth) is enabled. Ignored client cert auth for this url.", u.String())
				}
			}
		}
		if (u.Scheme == "https" || u.Scheme == "unixs") && cfg.ClientTLSInfo.Empty() {
			return nil, fmt.Errorf("TLS key/cert (--cert-file, --key-file) must be provided for client url %s with HTTPS scheme", u.String())
		}

		network := "tcp"
		addr := u.Host
		if u.Scheme == "unix" || u.Scheme == "unixs" {
			network = "unix"
			addr = u.Host + u.Path
		}
		sctx.network = network

		sctx.secure = u.Scheme == "https" || u.Scheme == "unixs"
		sctx.insecure = !sctx.secure
		if oldctx := sctxs[addr]; oldctx != nil {
			oldctx.secure = oldctx.secure || sctx.secure
			oldctx.insecure = oldctx.insecure || sctx.insecure
			continue
		}

		if sctx.l, err = net.Listen(network, addr); err != nil {
			return nil, err
		}
		// net.Listener will rewrite ipv4 0.0.0.0 to ipv6 [::], breaking
		// hosts that disable ipv6. So, use the address given by the user.
		sctx.addr = addr

		if fdLimit, fderr := runtimeutil.FDLimit(); fderr == nil {
			if fdLimit <= reservedInternalFDNum {
				if cfg.logger != nil {
					cfg.logger.Fatal(
						"file descriptor limit of etcd process is too low; please set higher",
						zap.Uint64("limit", fdLimit),
						zap.Int("recommended-limit", reservedInternalFDNum),
					)
				} else {
					plog.Fatalf("file descriptor limit[%d] of etcd process is too low, and should be set higher than %d to ensure internal usage", fdLimit, reservedInternalFDNum)
				}
			}
			sctx.l = transport.LimitListener(sctx.l, int(fdLimit-reservedInternalFDNum))
		}

		if network == "tcp" {
			if sctx.l, err = transport.NewKeepAliveListener(sctx.l, network, nil); err != nil {
				return nil, err
			}
		}

		defer func() {
			if err == nil {
				return
			}
			sctx.l.Close()
			if cfg.logger != nil {
				cfg.logger.Warn(
					"closing peer listener",
					zap.String("address", u.Host),
					zap.Error(err),
				)
			} else {
				plog.Info("stopping listening for client requests on ", u.Host)
			}
		}()
		for k := range cfg.UserHandlers {
			sctx.userHandlers[k] = cfg.UserHandlers[k]
		}
		sctx.serviceRegister = cfg.ServiceRegister
		if cfg.EnablePprof || cfg.Debug {
			sctx.registerPprof()
		}
		if cfg.Debug {
			sctx.registerTrace()
		}
		sctxs[addr] = sctx
	}
	return sctxs, nil
}

// 启动客户端监听Socket，等待客户端请求并响应
func (e *Etcd) serveClients() (err error) {
	fmt.Println("<===================>", "embed.serveClients \n")
	if !e.cfg.ClientTLSInfo.Empty() {
		if e.cfg.logger != nil {
			e.cfg.logger.Info(
				"starting with client TLS",
				zap.String("tls-info", fmt.Sprintf("%+v", e.cfg.ClientTLSInfo)),
				zap.Strings("cipher-suites", e.cfg.CipherSuites),
			)
		} else {
			plog.Infof("ClientTLS: %s", e.cfg.ClientTLSInfo)
		}
	}

	// Start a client server goroutine for each listen address
	var h http.Handler
	if e.Config().EnableV2 {
		if len(e.Config().ExperimentalEnableV2V3) > 0 {
			srv := v2v3.NewServer(e.cfg.logger, v3client.New(e.Server), e.cfg.ExperimentalEnableV2V3)
			h = v2http.NewClientHandler(e.GetLogger(), srv, e.Server.Cfg.ReqTimeout())
		} else {
			h = v2http.NewClientHandler(e.GetLogger(), e.Server, e.Server.Cfg.ReqTimeout())
		}
	} else {
		// 默认进入V3
		mux := http.NewServeMux()
		etcdhttp.HandleBasic(mux, e.Server)
		h = mux
	}

	gopts := []grpc.ServerOption{}
	if e.cfg.GRPCKeepAliveMinTime > time.Duration(0) {
		gopts = append(gopts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             e.cfg.GRPCKeepAliveMinTime,
			PermitWithoutStream: false,
		}))
	}
	if e.cfg.GRPCKeepAliveInterval > time.Duration(0) &&
		e.cfg.GRPCKeepAliveTimeout > time.Duration(0) {
		gopts = append(gopts, grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    e.cfg.GRPCKeepAliveInterval,
			Timeout: e.cfg.GRPCKeepAliveTimeout,
		}))
	}

	// start client servers in each goroutine
	// 针对每一个client地址创建一个服务监听协程(-listen-client-urls)
	for _, sctx := range e.sctxs {
		go func(s *serveCtx) {
			// 正式启动服务的地方
			// |-s.serve
			// | |- v3rpc.Server()
			// | | |- v3rpc.Server()
			// | | | |-pb.RegisterKVServer()
			e.errHandler(s.serve(e.Server, &e.cfg.ClientTLSInfo, h, e.errHandler, gopts...))
		}(sctx)
	}
	return nil
}

func (e *Etcd) serveMetrics() (err error) {
	if e.cfg.Metrics == "extensive" {
		grpc_prometheus.EnableHandlingTimeHistogram()
	}

	if len(e.cfg.ListenMetricsUrls) > 0 {
		metricsMux := http.NewServeMux()
		etcdhttp.HandleMetricsHealth(metricsMux, e.Server)

		for _, murl := range e.cfg.ListenMetricsUrls {
			tlsInfo := &e.cfg.ClientTLSInfo
			if murl.Scheme == "http" {
				tlsInfo = nil
			}
			ml, err := transport.NewListener(murl.Host, murl.Scheme, tlsInfo)
			if err != nil {
				return err
			}
			e.metricsListeners = append(e.metricsListeners, ml)
			go func(u url.URL, ln net.Listener) {
				if e.cfg.logger != nil {
					e.cfg.logger.Info(
						"serving metrics",
						zap.String("address", u.String()),
					)
				} else {
					plog.Info("listening for metrics on ", u.String())
				}
				e.errHandler(http.Serve(ln, metricsMux))
			}(murl, ml)
		}
	}
	return nil
}

func (e *Etcd) errHandler(err error) {
	select {
	case <-e.stopc:
		return
	default:
	}
	select {
	case <-e.stopc:
	case e.errc <- err:
	}
}

// GetLogger returns the logger.
func (e *Etcd) GetLogger() *zap.Logger {
	e.cfg.loggerMu.RLock()
	l := e.cfg.logger
	e.cfg.loggerMu.RUnlock()
	return l
}

func parseCompactionRetention(mode, retention string) (ret time.Duration, err error) {
	h, err := strconv.Atoi(retention)
	if err == nil {
		switch mode {
		case CompactorModeRevision:
			ret = time.Duration(int64(h))
		case CompactorModePeriodic:
			ret = time.Duration(int64(h)) * time.Hour
		}
	} else {
		// periodic compaction
		ret, err = time.ParseDuration(retention)
		if err != nil {
			return 0, fmt.Errorf("error parsing CompactionRetention: %v", err)
		}
	}
	return ret, nil
}
