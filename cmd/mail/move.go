package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var moveCmd = &cobra.Command{
	Use:   "move",
	Short: "Move a message to a folder",
	RunE:  runMove,
}

func init() {
	moveCmd.Flags().String("user", "", "user email (required)")
	_ = moveCmd.MarkFlagRequired("user")
	moveCmd.Flags().String("id", "", "message ID (required)")
	_ = moveCmd.MarkFlagRequired("id")
	moveCmd.Flags().String("folder", "", "destination folder ID (required)")
	_ = moveCmd.MarkFlagRequired("folder")
}

func runMove(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")
	folder, _ := cmd.Flags().GetString("folder")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	_, err = client.MoveMessage(ctx, &mailboxpb.MoveMessageRequest{
		UserId:    user,
		MessageId: id,
		FolderId:  folder,
	})
	if err != nil {
		return fmt.Errorf("move: %w", err)
	}
	fmt.Printf("message %s moved to %s\n", id, folder)
	return nil
}
