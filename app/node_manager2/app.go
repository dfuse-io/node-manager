// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodemanager

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dmetrics"
	nodeManager "github.com/dfuse-io/node-manager"
	"github.com/dfuse-io/node-manager/metrics"
	"github.com/dfuse-io/node-manager/mindreader"
	"github.com/dfuse-io/node-manager/operator"
	"github.com/dfuse-io/shutter"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type Config struct {
	GRPCAddr string
	HTTPAddr string

	// Backup Flags
	AutoBackupModulo        int
	AutoBackupPeriod        time.Duration
	AutoBackupHostnameMatch string // If non-empty, will only apply autobackup if we have that hostname

	// Snapshot Flags
	AutoSnapshotModulo        int
	AutoSnapshotPeriod        time.Duration
	AutoSnapshotHostnameMatch string // If non-empty, will only apply autosnapshot if we have that hostname

	// Volume Snapshot Flags
	AutoVolumeSnapshotModulo         int
	AutoVolumeSnapshotPeriod         time.Duration
	AutoVolumeSnapshotSpecificBlocks []uint64

	StartupDelay       time.Duration
	ConnectionWatchdog bool
}

type Modules struct {
	Operator                     *operator.Operator
	MetricsAndReadinessManager   *nodeManager.MetricsAndReadinessManager
	LaunchConnectionWatchdogFunc func(terminating <-chan struct{})
	MindreaderPlugin             *mindreader.MindReaderPlugin
	RegisterGRPCService          func(server *grpc.Server) error
	StartFailureHandlerFunc      func()
}

type App struct {
	*shutter.Shutter
	config  *Config
	modules *Modules
	zlogger *zap.Logger
}

func New(config *Config, modules *Modules, zlogger *zap.Logger) *App {
	return &App{
		Shutter: shutter.New(),
		config:  config,
		modules: modules,
		zlogger: zlogger,
	}
}

func (a *App) Run() error {
	hasMindreader := a.modules.MindreaderPlugin != nil
	a.zlogger.Info("running nodeos manager app", zap.Reflect("config", a.config), zap.Bool("mindreader", hasMindreader))

	hostname, _ := os.Hostname()
	a.zlogger.Info("retrieved hostname from os", zap.String("hostname", hostname))

	dmetrics.Register(metrics.NodeosMetricset)
	dmetrics.Register(metrics.Metricset)

	if a.config.AutoBackupPeriod != 0 || a.config.AutoBackupModulo != 0 {
		a.modules.Operator.ConfigureAutoBackup(a.config.AutoBackupPeriod, a.config.AutoBackupModulo, a.config.AutoBackupHostnameMatch, hostname)
	}

	if a.config.AutoSnapshotPeriod != 0 || a.config.AutoSnapshotModulo != 0 {
		a.modules.Operator.ConfigureAutoSnapshot(a.config.AutoSnapshotPeriod, a.config.AutoSnapshotModulo, a.config.AutoSnapshotHostnameMatch, hostname)
	}

	if a.config.AutoVolumeSnapshotPeriod != 0 || a.config.AutoVolumeSnapshotModulo != 0 || len(a.config.AutoVolumeSnapshotSpecificBlocks) > 0 {
		a.modules.Operator.ConfigureAutoVolumeSnapshot(a.config.AutoVolumeSnapshotPeriod, a.config.AutoVolumeSnapshotModulo, a.config.AutoVolumeSnapshotSpecificBlocks)
	}

	a.OnTerminating(func(err error) {
		a.modules.Operator.Shutdown(err)
		<-a.modules.Operator.Terminated()
	})

	a.modules.Operator.OnTerminated(func(err error) {
		a.zlogger.Info("chain operator terminated shutting down mindreader app")
		a.Shutdown(err)
	})

	if a.config.StartupDelay != 0 {
		time.Sleep(a.config.StartupDelay)
	}

	var httpOptions []operator.HTTPOption
	if hasMindreader {
		if err := a.startMindreader(); err != nil {
			return fmt.Errorf("unable to start mindreader: %w", err)
		}

		if a.modules.MindreaderPlugin.HasContinuityChecker() {
			httpOptions = append(httpOptions, func(r *mux.Router) {
				r.HandleFunc("/v1/reset_cc", func(w http.ResponseWriter, _ *http.Request) {
					a.modules.MindreaderPlugin.ResetContinuityChecker()
					w.Write([]byte("ok"))
				})
			})
		}
	}

	a.zlogger.Info("launching operator")
	go a.modules.MetricsAndReadinessManager.Launch()
	go a.Shutdown(a.modules.Operator.Launch(a.config.HTTPAddr, httpOptions...))

	if a.config.ConnectionWatchdog {
		go a.modules.LaunchConnectionWatchdogFunc(a.Terminating())
	}

	return nil
}

func (a *App) IsReady() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	url := fmt.Sprintf("http://%s/healthz", a.config.HTTPAddr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		a.zlogger.Warn("unable to build get health request", zap.Error(err))
		return false
	}

	client := http.DefaultClient
	res, err := client.Do(req)
	if err != nil {
		a.zlogger.Debug("unable to execute get health request", zap.Error(err))
		return false
	}

	return res.StatusCode == 200
}

func (a *App) startMindreader() error {
	a.zlogger.Info("starting mindreader gRPC server")
	gs := dgrpc.NewServer(dgrpc.WithLogger(a.zlogger))

	if a.modules.RegisterGRPCService != nil {
		err := a.modules.RegisterGRPCService(gs)
		if err != nil {
			return fmt.Errorf("register extra grpc service: %w", err)
		}
	}

	err := mindreader.RunGRPCServer(gs, a.config.GRPCAddr, a.zlogger)
	if err != nil {
		return err
	}

	a.zlogger.Info("launching mindreader plugin")
	go a.modules.MindreaderPlugin.Launch()
	return nil
}
