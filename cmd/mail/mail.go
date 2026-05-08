// Package mail provides the "postbox mail" subcommand tree for interacting
// with user mailboxes through the Postbox gRPC server.
package mail

import (
	"context"
	"fmt"
	"time"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Command returns the "mail" cobra command with all sub-commands attached.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mail",
		Short: "Read and write user mailboxes",
	}
	cmd.AddCommand(listCmd)
	cmd.AddCommand(readCmd)
	cmd.AddCommand(sendCmd)
	cmd.AddCommand(trashCmd)
	cmd.AddCommand(restoreCmd)
	cmd.AddCommand(deleteCmd)
	cmd.AddCommand(moveCmd)
	cmd.AddCommand(tagCmd)
	cmd.AddCommand(foldersCmd)
	cmd.AddCommand(statsCmd)
	return cmd
}

// dial opens an insecure gRPC connection to addr and returns both the
// connection and a ready-to-use MailboxServiceClient.
func dial(addr string) (*grpc.ClientConn, mailboxpb.MailboxServiceClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return conn, mailboxpb.NewMailboxServiceClient(conn), nil
}

func timeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func rootAddr(cmd *cobra.Command) string {
	addr, _ := cmd.Root().PersistentFlags().GetString("addr")
	return addr
}
