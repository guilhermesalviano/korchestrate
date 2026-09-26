package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/web"
	"github.com/spf13/cobra"
)

func newWebCmd(configPath, repo, artifactsDir *string) *cobra.Command {
	var listen string
	var allowDirty bool
	cmd := &cobra.Command{
		Use: "web", Short: "serve the dashboard for phones and browsers on your LAN", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig(*configPath, *repo, *artifactsDir)
			if err != nil {
				return err
			}
			token, err := web.NewToken()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			app, err := web.New(ctx, cfg, pipeline.Options{AllowDirty: allowDirty}, token)
			if err != nil {
				return err
			}
			app.PlanFromPrompt = func(prompt string) (*contracts.Plan, string, error) { return planFiles(prompt, nil) }
			app.LoadRunConfig = func(run *artifact.Run) (*config.Config, error) { return loadRunConfig(run, *configPath) }
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if err := app.Shutdown(cleanup); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "kor: waiting for agents to stop:", err)
				}
			}()
			listener, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			server := &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
			fmt.Fprintln(cmd.OutOrStdout(), "kor web —", cfg.Repo)
			for _, base := range browserURLs(listener.Addr()) {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+base+"/#token="+token)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Open a LAN link on your phone while connected to the same Wi-Fi. Keep this access link private; it controls agents on this computer.\nLeave this command running. Ctrl+C stops the server and its active run.")
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			select {
			case <-ctx.Done():
				shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := server.Shutdown(shutdown); err != nil {
					_ = server.Close()
					return err
				}
				return nil
			case err := <-done:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}
				return err
			}
		},
	}
	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:8787", "listen address (use 127.0.0.1:8787 for this computer only)")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow runs in a repository with uncommitted changes")
	return cmd
}

func browserURLs(addr net.Addr) []string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsUnspecified() {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{"http://" + net.JoinHostPort("127.0.0.1", port)}
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok || network.IP.IsLoopback() || network.IP.To4() == nil {
			continue
		}
		urls = append(urls, "http://"+net.JoinHostPort(network.IP.String(), port))
	}
	return urls
}
