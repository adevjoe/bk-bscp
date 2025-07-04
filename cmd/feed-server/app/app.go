/*
 * Tencent is pleased to support the open source community by making Blueking Container Service available.
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package app NOTES
package app

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Tencent/bk-bcs/bcs-common/common/tcp/listener"
	grpc_recovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/realip"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/TencentBlueKing/bk-bscp/cmd/feed-server/options"
	"github.com/TencentBlueKing/bk-bscp/cmd/feed-server/service"
	"github.com/TencentBlueKing/bk-bscp/internal/ratelimiter"
	"github.com/TencentBlueKing/bk-bscp/internal/runtime/brpc"
	"github.com/TencentBlueKing/bk-bscp/internal/runtime/ctl"
	"github.com/TencentBlueKing/bk-bscp/internal/runtime/shutdown"
	"github.com/TencentBlueKing/bk-bscp/internal/serviced"
	"github.com/TencentBlueKing/bk-bscp/pkg/cc"
	"github.com/TencentBlueKing/bk-bscp/pkg/criteria/uuid"
	"github.com/TencentBlueKing/bk-bscp/pkg/logs"
	"github.com/TencentBlueKing/bk-bscp/pkg/metrics"
	pbfs "github.com/TencentBlueKing/bk-bscp/pkg/protocol/feed-server"
	"github.com/TencentBlueKing/bk-bscp/pkg/tools"
)

// Run start the feed server
func Run(opt *options.Option) error {
	fs := new(feedServer)
	if err := fs.prepare(opt); err != nil {
		return err
	}

	if err := fs.listenAndServe(); err != nil {
		return err
	}

	if err := fs.service.ListenAndServeRest(); err != nil {
		return err
	}

	if err := fs.service.ListenAndGwServerRest(); err != nil {
		return err
	}

	if err := fs.register(); err != nil {
		return err
	}

	shutdown.RegisterFirstShutdown(fs.finalizer)
	shutdown.WaitShutdown(20)
	return nil
}

type feedServer struct {
	serve   *grpc.Server
	sd      serviced.ServiceDiscover
	service *service.Service
}

// prepare do prepare jobs before run feed server.
func (fs *feedServer) prepare(opt *options.Option) error {
	// load settings from config file.
	if err := cc.LoadSettings(opt.Sys); err != nil {
		return fmt.Errorf("load settings from config files failed, err: %v", err)
	}

	logs.InitLogger(cc.FeedServer().Log.Logs())
	logs.Infof("load settings from config file success.")
	logs.Infof("current service name: %s", opt.Name)

	// init metrics
	metrics.InitMetrics(net.JoinHostPort(cc.FeedServer().Network.BindIP,
		strconv.Itoa(int(cc.FeedServer().Network.RpcPort))))

	etcdOpt, err := cc.FeedServer().Service.Etcd.ToConfig()
	if err != nil {
		return fmt.Errorf("get etcd config failed, err: %v", err)
	}

	// register data service.
	svcOpt := serviced.ServiceOption{
		Name: cc.FeedServerName,
		IP:   cc.FeedServer().Network.BindIP,
		Port: cc.FeedServer().Network.RpcPort,
		Uid:  uuid.UUID(),
	}
	sd, err := serviced.NewServiceD(etcdOpt, svcOpt)
	if err != nil {
		return fmt.Errorf("new service discovery failed, err: %v", err)
	}

	fs.sd = sd

	// init bscp control tool
	if err = ctl.LoadCtl(ctl.WithBasics(sd)...); err != nil {
		return fmt.Errorf("load control tool failed, err: %v", err)
	}

	svc, err := service.NewService(fs.sd, opt.Name)
	if err != nil {
		return fmt.Errorf("initialize service failed, err: %v", err)
	}
	fs.service = svc

	return nil
}

// listenAndServe listen the grpc serve and set up the shutdown gracefully job.
// nolint:funlen
func (fs *feedServer) listenAndServe() error {
	// generate standard grpc server grpcMetrics.
	grpcMetrics := grpc_prometheus.NewServerMetrics()
	grpcMetrics.EnableHandlingTimeHistogram(metrics.GrpcBuckets)
	recoveryOpt := grpc_recovery.WithRecoveryHandlerContext(brpc.RecoveryHandlerFuncContext)

	// 基于client realIP的全局限流器
	ipLimit := cc.FeedServer().RateLimiter.IP.Limit
	if ipLimit == 0 {
		ipLimit = ratelimiter.DefaultIPLimit // 设置默认值，防止配置错误
	}
	ipBurst := cc.FeedServer().RateLimiter.IP.Burst
	if ipBurst == 0 {
		ipBurst = ratelimiter.DefaultIPLimit * 2 // 设置默认值，防止配置错误
	}
	ipLimiter := ratelimiter.NewGlobalRL(ipLimit, ipBurst)

	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(1 * 1024 * 1024),
		// add bscp unary interceptor and standard grpc server metrics interceptor.
		grpc.ChainUnaryInterceptor(
			realip.UnaryServerInterceptorOpts(),
			service.LogUnaryServerInterceptor(),
			grpcMetrics.UnaryServerInterceptor(),
			ratelimit.UnaryServerInterceptor(ipLimiter),
			service.FeedUnaryAuthInterceptor,
			service.FeedUnaryUpdateLastConsumedTimeInterceptor,
			grpc_recovery.UnaryServerInterceptor(recoveryOpt),
		),
		grpc.ChainStreamInterceptor(
			realip.StreamServerInterceptorOpts(),
			grpcMetrics.StreamServerInterceptor(),
			ratelimit.StreamServerInterceptor(ipLimiter),
			service.FeedStreamAuthInterceptor,
			grpc_recovery.StreamServerInterceptor(recoveryOpt),
		),
	}

	network := cc.FeedServer().Network
	if network.TLS.Enable() {
		tls := network.TLS
		tlsC, err := tools.ClientTLSConfVerify(tls.InsecureSkipVerify, tls.CAFile, tls.CertFile, tls.KeyFile,
			tls.Password)
		if err != nil {
			return fmt.Errorf("init tls config failed, err: %v", err)
		}

		cred := credentials.NewTLS(tlsC)
		opts = append(opts, grpc.Creds(cred))
		// set keepalive params so that feed-proxy could maintain a grpc connection pool
		opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// a bit less than feed-proxy's grpc keepalive time 30s
			MinTime:             25 * time.Second,
			PermitWithoutStream: true,
		}))
	}

	serve := grpc.NewServer(opts...)
	pbfs.RegisterUpstreamServer(serve, fs.service)
	// Register reflection service on gRPC server.
	reflection.Register(serve)

	// initialize and register standard grpc server grpcMetrics.
	grpcMetrics.InitializeMetrics(serve)
	if err := metrics.Register().Register(grpcMetrics); err != nil {
		return fmt.Errorf("register metrics failed, err: %v", err)
	}

	fs.serve = serve

	go func() {
		notifier := shutdown.AddNotifier()
		<-notifier.Signal
		logs.Infof("start shutdown feed server grpc server gracefully...")

		fs.serve.GracefulStop()
		notifier.Done()

		logs.Infof("shutdown feed server grpc server success...")
	}()

	addr := tools.GetListenAddr(network.BindIP, int(network.RpcPort))
	addrs := tools.GetListenAddrs(network.BindIPs, int(network.RpcPort))
	dualStackListener := listener.NewDualStackListener()
	if err := dualStackListener.AddListenerWithAddr(addr); err != nil {
		return err
	}
	logs.Infof("grpc server listen address: %s", addr)

	for _, a := range addrs {
		if a == addr {
			continue
		}
		if err := dualStackListener.AddListenerWithAddr(a); err != nil {
			return err
		}
		logs.Infof("grpc server listen address: %s", a)
	}

	go func() {
		if err := serve.Serve(dualStackListener); err != nil {
			logs.Errorf("serve grpc server failed, err: %v", err)
			shutdown.SignalShutdownGracefully()
		}
	}()

	return nil
}

func (fs *feedServer) finalizer() {

	if err := fs.sd.Deregister(); err != nil {
		logs.Errorf("process service shutdown, but deregister failed, err: %v", err)
		return
	}

	logs.Infof("shutting down service, deregister service success.")
}

// register the grpc serve.
func (fs *feedServer) register() error {
	if err := fs.sd.Register(); err != nil {
		return fmt.Errorf("register feed server failed, err: %v", err)
	}

	logs.Infof("register feed server to etcd success.")
	return nil
}
