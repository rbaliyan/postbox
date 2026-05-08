package cmd

import (
	"context"
	"fmt"
	"time"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of the running Postbox node",
	RunE:  runStatus,
}

func runStatus(cmd *cobra.Command, _ []string) error {
	addr, _ := cmd.Root().PersistentFlags().GetString("addr")

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck

	client := postboxpb.NewPostboxServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}
	mailboxStatus := "disconnected"
	if resp.GetMailboxConnected() {
		mailboxStatus = "connected"
	}
	fmt.Printf("node_id:  %s\nmode:     %s\ndomains:  %d\nusers:    %d\nmailbox:  %s\n",
		resp.GetNodeId(), resp.GetMode(), resp.GetDomainCount(),
		resp.GetUserCount(), mailboxStatus)
	return nil
}
