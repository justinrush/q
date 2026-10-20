package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/justinrush/q/internal/analytics"
	analyticsweb "github.com/justinrush/q/web/analytics"
	"github.com/spf13/cobra"
)

func buildAnalyticsSubcommand() *cobra.Command {
	var port int
	var noOpen bool
	cmd := &cobra.Command{Use: "analytics", Short: "Explore recorded mission costs in a local browser", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		assets, err := analyticsweb.Assets()
		if err != nil {
			return err
		}
		if port < 0 || port > 65535 {
			return fmt.Errorf("port must be between 0 and 65535")
		}
		client, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return err
		}
		defer func() { _ = listener.Close() }()
		host := listener.Addr().String()
		address := "http://" + host
		server := &http.Server{Handler: analytics.Handler(client, assets, host), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), address+" (Ctrl-C to stop)")
		if !noOpen {
			opener := "xdg-open"
			if runtime.GOOS == "darwin" {
				opener = "open"
			}
			go func() {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				_ = exec.CommandContext(ctx, opener, address).Run()
			}()
		}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		select {
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-cmd.Context().Done():
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(ctx)
		}
	}}
	cmd.Flags().IntVar(&port, "port", 0, "Loopback port (0 selects an available port)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the URL without opening a browser")
	return cmd
}
