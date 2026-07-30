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
	defer log.Sync()

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	agent, err := sched.NewAgent(ctx, log, nodeName, metricsAddr)
	if err != nil {
		log.Fatal("failed to create agent", zap.Error(err))
	}

	if err := agent.Run(ctx); err != nil {
		log.Fatal("agent failed", zap.Error(err))
	}

	log.Info("agent stopped")
}
