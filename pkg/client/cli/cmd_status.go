package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/rpc/v2/daemon"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/cliutil"
	"github.com/telepresenceio/telepresence/v2/pkg/iputil"
)

func statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:  "status",
		Args: cobra.NoArgs,

		Short: "Show connectivity status",
		RunE:  status,
	}
}

// status will retrieve connectivity status from the daemon and print it on stdout.
func status(cmd *cobra.Command, _ []string) error {
	if err := daemonStatus(cmd); err != nil {
		return err
	}

	if err := connectorStatus(cmd); err != nil {
		return err
	}

	return nil
}

func daemonStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	err := cliutil.WithStartedNetwork(cmd.Context(), func(ctx context.Context, daemonClient daemon.DaemonClient) error {
		var err error
		status, err := daemonClient.Status(cmd.Context(), &empty.Empty{})
		if err != nil {
			return err
		}
		version, err := daemonClient.Version(cmd.Context(), &empty.Empty{})
		if err != nil {
			return err
		}

		fmt.Fprintln(out, "Root Daemon: Running")
		fmt.Fprintf(out, "  Version   : %s (api %d)\n", version.Version, version.ApiVersion)
		if obc := status.OutboundConfig; obc != nil {
			dns := obc.Dns
			fmt.Fprintf(out, "  DNS       :\n")
			if dns.LocalIp != nil {
				// Local IP is only set when the overriding resolver is used
				fmt.Fprintf(out, "    Local IP        : %v\n", net.IP(dns.LocalIp))
			}
			fmt.Fprintf(out, "    Remote IP       : %v\n", net.IP(dns.RemoteIp))
			fmt.Fprintf(out, "    Exclude suffixes: %v\n", dns.ExcludeSuffixes)
			fmt.Fprintf(out, "    Include suffixes: %v\n", dns.IncludeSuffixes)
			fmt.Fprintf(out, "    Timeout         : %v\n", dns.LookupTimeout.AsDuration())
			fmt.Fprintf(out, "  Also Proxy : (%d subnets)\n", len(obc.AlsoProxySubnets))
			fmt.Fprintf(out, "  Never Proxy: (%d subnets)\n", len(obc.NeverProxySubnets))
			for _, subnet := range obc.AlsoProxySubnets {
				fmt.Fprintf(out, "    - %s\n", iputil.IPNetFromRPC(subnet))
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, cliutil.ErrNoNetwork) {
			fmt.Fprintln(out, "Root Daemon: Not running")
			return nil
		}
		return err
	}
	return nil
}

func connectorStatus(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	err := cliutil.WithStartedConnector(cmd.Context(), false, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
		fmt.Fprintln(out, "User Daemon: Running")

		type kv struct {
			Key   string
			Value string
		}
		var fields []kv
		defer func() {
			klen := 0
			for _, kv := range fields {
				if len(kv.Key) > klen {
					klen = len(kv.Key)
				}
			}
			for _, kv := range fields {
				vlines := strings.Split(strings.TrimSpace(kv.Value), "\n")
				fmt.Fprintf(out, "  %-*s: %s\n", klen, kv.Key, vlines[0])
				for _, vline := range vlines[1:] {
					fmt.Fprintf(out, "    %s\n", vline)
				}
			}
		}()

		version, err := connectorClient.Version(ctx, &empty.Empty{})
		if err != nil {
			return err
		}
		fields = append(fields, kv{"Version", fmt.Sprintf("%s (api %d)", version.Version, version.ApiVersion)})
		fields = append(fields, kv{"Executable", version.Executable})

		if !cliutil.HasLoggedIn(ctx) {
			fields = append(fields, kv{"Ambassador Cloud", "Logged out"})
		} else if _, err := cliutil.GetCloudUserInfo(ctx, false, true); err != nil {
			fields = append(fields, kv{"Ambassador Cloud", "Login expired (or otherwise no-longer-operational)"})
		} else {
			fields = append(fields, kv{"Ambassador Cloud", "Logged in"})
		}

		status, err := connectorClient.Status(ctx, &empty.Empty{})
		if err != nil {
			return err
		}
		switch status.Error {
		case connector.ConnectInfo_UNSPECIFIED, connector.ConnectInfo_ALREADY_CONNECTED:
			fields = append(fields, kv{"Status", "Connected"})
		case connector.ConnectInfo_MUST_RESTART:
			fields = append(fields, kv{"Status", "Connected, but must restart"})
		case connector.ConnectInfo_DISCONNECTED:
			fields = append(fields, kv{"Status", "Not connected"})
			return nil
		case connector.ConnectInfo_CLUSTER_FAILED:
			fields = append(fields, kv{"Status", "Not connected, error talking to cluster"})
			fields = append(fields, kv{"Error", status.ErrorText})
			return nil
		case connector.ConnectInfo_TRAFFIC_MANAGER_FAILED:
			fields = append(fields, kv{"Status", "Not connected, error talking to in-cluster Telepresence traffic-manager"})
			fields = append(fields, kv{"Error", status.ErrorText})
			return nil
		}
		fields = append(fields, kv{"Kubernetes server", status.ClusterServer})
		fields = append(fields, kv{"Kubernetes context", status.ClusterContext})
		intercepts := fmt.Sprintf("%d total\n", len(status.GetIntercepts().GetIntercepts()))
		for _, icept := range status.GetIntercepts().GetIntercepts() {
			intercepts += fmt.Sprintf("%s: %s\n", icept.Spec.Name, icept.Spec.Client)
		}
		fields = append(fields, kv{"Intercepts", intercepts})

		return nil
	})
	if err != nil {
		if errors.Is(err, cliutil.ErrNoUserDaemon) {
			fmt.Fprintln(out, "User Daemon: Not running")
			return nil
		}
		return err
	}
	return nil
}
