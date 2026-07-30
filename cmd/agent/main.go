package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

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

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	agent, err := sched.NewAgent(ctx, log, nodeName, metricsAddr)
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
