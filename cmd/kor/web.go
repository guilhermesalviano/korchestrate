package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/guilhermesalviano/korchestrate/internal/artifact"
	"github.com/guilhermesalviano/korchestrate/internal/config"
	"github.com/guilhermesalviano/korchestrate/internal/contracts"
	"github.com/guilhermesalviano/korchestrate/internal/pipeline"
	"github.com/guilhermesalviano/korchestrate/internal/web"
	"github.com/spf13/cobra"
)

const defaultWebListen = "0.0.0.0:8787"

// webServer is a running dashboard server shared by `kor web` and the TUI.
type webServer struct {
	app    *web.Server
	server *http.Server
	links  []string // dashboard links, loopback first
	Done   chan error
}

// startWeb serves the web dashboard on listen until Stop is called or ctx ends.
func startWeb(ctx context.Context, cfg *config.Config, configPath, listen string, allowDirty bool) (*webServer, error) {
	app, err := web.New(ctx, cfg, pipeline.Options{AllowDirty: allowDirty})
	if err != nil {
		return nil, err
	}
	app.PlanFromPrompt = func(prompt string) (*contracts.Plan, string, error) { return planFiles(prompt, nil) }
	app.LoadRunConfig = func(run *artifact.Run) (*config.Config, error) { return loadRunConfig(run, configPath) }
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		_ = app.Shutdown(context.Background())
		return nil, err
	}
	ws := &webServer{
		app:    app,
		server: &http.Server{Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10},
		Done:   make(chan error, 1),
	}
	for _, base := range browserURLs(listener.Addr()) {
		ws.links = append(ws.links, base+"/")
	}
	go func() {
		err := ws.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		ws.Done <- err
	}()
	return ws, nil
}

// Links returns the dashboard links; the LAN link is last.
func (ws *webServer) Links() []string { return ws.links }

// Active reports whether a browser-started run is in flight.
func (ws *webServer) Active() bool { return ws.app.Active() }

// Stop closes the listener, then cancels agents and waits for them to exit.
func (ws *webServer) Stop(ctx context.Context) error {
	shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.server.Shutdown(shutdown); err != nil {
		_ = ws.server.Close()
	}
	cleanup, cancelCleanup := context.WithTimeout(ctx, 15*time.Second)
	defer cancelCleanup()
	if err := ws.app.Shutdown(cleanup); err != nil {
		return fmt.Errorf("waiting for agents to stop: %w", err)
	}
	return nil
}

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
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ws, err := startWeb(ctx, cfg, *configPath, listen, allowDirty)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "kor web —", cfg.Repo)
			for _, link := range ws.links {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+link)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Open the LAN link on your phone while connected to the same Wi-Fi. Only this computer and devices on your local network can connect, with no token, so anyone on that network can control agents here.\nLeave this command running. Ctrl+C stops the server and its active run.")
			var serveErr error
			select {
			case <-ctx.Done():
			case serveErr = <-ws.Done:
			}
			if err := ws.Stop(context.Background()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "kor:", err)
			}
			return serveErr
		},
	}
	cmd.Flags().StringVar(&listen, "listen", defaultWebListen, "listen address (use 127.0.0.1:8787 for this computer only)")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow runs in a repository with uncommitted changes")
	return cmd
}

// virtualInterfaces are container and VM bridges a phone cannot reach.
var virtualInterfaces = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "cni", "flannel", "podman", "lxc", "lxd"}

// browserURLs lists the dashboard links: loopback first and the LAN address
// used for the default route last, since that is the one to open on a phone.
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
	primary := primaryIP()
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || isVirtual(iface.Name) {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil || !network.IP.IsPrivate() || network.IP.Equal(primary) {
				continue
			}
			urls = append(urls, "http://"+net.JoinHostPort(network.IP.String(), port))
		}
	}
	if primary != nil && primary.IsPrivate() {
		urls = append(urls, "http://"+net.JoinHostPort(primary.String(), port))
	}
	return urls
}

func isVirtual(name string) bool {
	for _, prefix := range virtualInterfaces {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// primaryIP returns the local address of the default route. Connecting a UDP
// socket only selects a route; no packet is sent.
func primaryIP() net.IP {
	conn, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP
	}
	return nil
}
