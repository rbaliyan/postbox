package cmd

import (
	"context"
	"fmt"
	"time"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var discoverCmd = &cobra.Command{
	Use:   "discover <target>",
	Short: "Discover which node owns a domain or user email",
	Args:  cobra.ExactArgs(1),
	RunE:  runDiscover,
}

func runDiscover(cmd *cobra.Command, args []string) error {
	addr, _ := cmd.Root().PersistentFlags().GetString("addr")

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck

	client := postboxpb.NewPostboxServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.Discover(ctx, &postboxpb.DiscoverRequest{Target: args[0]})
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	fmt.Printf("node_id: %s\naddress: %s\nmode:    %s\n",
		resp.GetNodeId(), resp.GetAddress(), resp.GetMode())
	return nil
}
