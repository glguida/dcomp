package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/glguida/dcomp/proxy"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	flags := flag.NewFlagSet("dcomp-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "resolved proxy configuration")
	readyFD := flags.Int("ready-fd", -1, "inherited readiness file descriptor")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *configPath == "" {
		fmt.Fprintln(os.Stderr, "dcomp-proxy: --config FILE is required")
		return 2
	}
	if *readyFD != -1 && *readyFD < 3 {
		fmt.Fprintln(os.Stderr, "dcomp-proxy: --ready-fd must be at least 3")
		return 2
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dcomp-proxy:", err)
		return 1
	}
	config, err := proxy.LoadConfig(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dcomp-proxy:", err)
		return 1
	}
	var readyFile *os.File
	if *readyFD >= 3 {
		readyFile = os.NewFile(uintptr(*readyFD), "dcomp-proxy-ready")
		if readyFile == nil {
			fmt.Fprintln(os.Stderr, "dcomp-proxy: invalid readiness descriptor")
			return 2
		}
		defer readyFile.Close()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = proxy.Run(ctx, config, func(proxy.Status) error {
		if readyFile == nil {
			return nil
		}
		if _, err := readyFile.Write([]byte{1}); err != nil {
			return err
		}
		return readyFile.Close()
	})
	if err != nil {
		log.Printf("dcomp-proxy: %v", err)
		return 1
	}
	return 0
}
