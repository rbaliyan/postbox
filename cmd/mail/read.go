package mail

import (
	"fmt"
	"strings"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var readCmd = &cobra.Command{
	Use:   "read",
	Short: "Read a message by ID",
	RunE:  runRead,
}

func init() {
	readCmd.Flags().String("user", "", "user email (required)")
	_ = readCmd.MarkFlagRequired("user")
	readCmd.Flags().String("id", "", "message ID (required)")
	_ = readCmd.MarkFlagRequired("id")
}

func runRead(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	resp, err := client.GetMessage(ctx, &mailboxpb.GetMessageRequest{
		UserId:    user,
		MessageId: id,
	})
	if err != nil {
		return fmt.Errorf("get message: %w", err)
	}

	msg := resp.GetMessage()
	fmt.Printf("ID:        %s\nFrom:      %s\nTo:        %s\nSubject:   %s\nDate:      %s\nRead:      %v\nFolder:    %s\n",
		msg.GetId(),
		msg.GetSenderId(),
		strings.Join(msg.GetRecipientIds(), ", "),
		msg.GetSubject(),
		msg.GetCreatedAt().AsTime().Format("2006-01-02 15:04:05 MST"),
		msg.GetIsRead(),
		msg.GetFolderId(),
	)
	if len(msg.GetTags()) > 0 {
		fmt.Printf("Tags:      %s\n", strings.Join(msg.GetTags(), ", "))
	}
	fmt.Printf("\n%s\n", msg.GetBody())
	return nil
}
