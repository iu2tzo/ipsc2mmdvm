package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/iu2tzo/ipsc2mmdvm/internal/config"
	"github.com/iu2tzo/ipsc2mmdvm/internal/ipsc"
	"github.com/iu2tzo/ipsc2mmdvm/internal/metrics"
	"github.com/iu2tzo/ipsc2mmdvm/internal/mmdvm"
	"github.com/iu2tzo/ipsc2mmdvm/internal/timeslot"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"github.com/ztrue/shutdown"
)

func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ipsc2mmdvm",
		Version: fmt.Sprintf("%s - %s", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		RunE:              runRoot,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
	}
	config.RegisterFlags(cmd.Flags(), "config.yaml")
	return cmd
}

func runRoot(cmd *cobra.Command, _ []string) error {
	fmt.Printf("ipsc2mmdvm - %s (%s)\n", cmd.Annotations["version"], cmd.Annotations["commit"])

	cfg, err := config.Load(cmd.Flags(), "config.yaml")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var logger *slog.Logger
	switch cfg.LogLevel {
	case config.LogLevelDebug:
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	// Create metrics and optionally start the metrics HTTP server.
	var m *metrics.Metrics
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled && cfg.Metrics.Address != "" {
		m = metrics.NewMetrics()
		mux := http.NewServeMux()
		mux.Handle("/metrics", m.Handler())
		metricsSrv = &http.Server{
			Addr:    cfg.Metrics.Address,
			Handler: mux,
		}
		go func() {
			slog.Info("Starting metrics server", "address", cfg.Metrics.Address)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Metrics server error", "error", err)
			}
		}()
	}

	// Create one MMDVM client per configured network (DMR master).
	// All clients share a single outbound timeslot manager so that
	// only one master can feed a given timeslot toward IPSC at a time.
	outboundTSMgr := timeslot.NewManager()
	if m != nil {
		outboundTSMgr.SetMetrics(m, "outbound")
	}
	// Clients are constructed here but only started immediately when
	// require-repeater is disabled; otherwise their lifecycle is driven
	// by the IPSC server's peer-connection callback wired in below.
	mmdvmClients := make([]*mmdvm.MMDVMClient, 0, len(cfg.MMDVM))
	for i := range cfg.MMDVM {
		client := mmdvm.NewMMDVMClient(&cfg.MMDVM[i], m)
		client.SetOutboundTSManager(outboundTSMgr)
		mmdvmClients = append(mmdvmClients, client)
	}

	ipscServer := ipsc.NewIPSCServer(cfg, m)

	ipscServer.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
		for _, client := range mmdvmClients {
			if client.MatchesRules(packetType, data, false) {
				dataCopy := make([]byte, len(data))
				copy(dataCopy, data)
				client.HandleIPSCBurst(packetType, dataCopy, addr)
				return
			}
		}
		for _, client := range mmdvmClients {
			if client.MatchesRules(packetType, data, true) {
				dataCopy := make([]byte, len(data))
				copy(dataCopy, data)
				client.HandleIPSCBurst(packetType, dataCopy, addr)
				return
			}
		}
	})

	// Wire all MMDVM clients' inbound data to the IPSC server.
	for _, client := range mmdvmClients {
		client.SetIPSCHandler(ipscServer.SendUserPacket)
	}

	if cfg.IPSC.RequireRepeater {
		// Gate the DMR network connections on repeater presence: only
		// open the MMDVM connections once a repeater peer registers
		// with the IPSC server, and tear them down again once that
		// repeater goes stale (see IPSCServer.reapStalePeers).
		ipscServer.SetPeerConnectionHandler(func(connected bool) {
			if connected {
				slog.Info("repeater connected, starting DMR network connections")
				for _, client := range mmdvmClients {
					if startErr := client.Start(); startErr != nil {
						slog.Error("failed to start MMDVM client", "network", client.Name(), "error", startErr)
					}
				}
			} else {
				slog.Info("repeater disconnected, stopping DMR network connections")
				for _, client := range mmdvmClients {
					client.Stop()
				}
			}
		})
	} else {
		// Default behaviour: connect to all configured DMR networks
		// immediately at startup, regardless of repeater state.
		for _, client := range mmdvmClients {
			if err = client.Start(); err != nil {
				return fmt.Errorf("failed to start MMDVM client %q: %w", client.Name(), err)
			}
		}
	}

	err = ipscServer.Start()
	if err != nil {
		return fmt.Errorf("failed to start IPSC server: %w", err)
	}

	stop := func(sig os.Signal) {
		slog.Info("received signal, shutting down...", "signal", sig.String())

		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(context.Background()); err != nil {
				slog.Error("Error shutting down metrics server", "error", err)
			}
		}

		ipscServer.Stop()
		for _, client := range mmdvmClients {
			client.Stop()
		}
	}

	shutdown.AddWithParam(stop)
	shutdown.Listen(syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	return nil
}
