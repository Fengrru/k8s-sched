package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fengrru/k8s-sched/internal/sched"
	"go.uber.org/zap"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		nodeName    string
		metricsAddr string
	)
	flag.StringVar(&nodeName, "node", os.Getenv("NODE_NAME"), "Kubernetes node name")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090", "Prometheus metrics listen address")
	flag.Parse()

	if nodeName == "" {
		nodeName, _ = os.Hostname()
	}

	// HOST_PROC handling lives in internal/maps: it probes /host/proc
	// at package init, so no env juggling is needed here.

	log, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	// SIGTERM triggers a graceful handover: the agent keeps the
	// sched_ext scheduler attached for a short window so a replacement
	// pod (rolling upgrade) can attach before we exit, avoiding an
	// EEVDF fallback gap. A second signal bails out immediately.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		if sig == syscall.SIGTERM {
			select {
			case <-time.After(sched.HandoffDelay):
			case <-sigCh:
			}
		}
		cancel()
	}()

	agent, err := sched.NewAgent(log, nodeName, metricsAddr)
	if err != nil {
		log.Error("failed to create agent", zap.Error(err))
		return 1
	}

	if err := agent.Run(ctx); err != nil {
		log.Error("agent failed", zap.Error(err))
		return 1
	}

	log.Info("agent stopped")
	return 0
}
