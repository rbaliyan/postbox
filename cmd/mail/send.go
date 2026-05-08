package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send a message",
	RunE:  runSend,
}

func init() {
	sendCmd.Flags().String("from", "", "sender user email (required)")
	_ = sendCmd.MarkFlagRequired("from")
	sendCmd.Flags().StringArray("to", nil, "recipient email(s) (required)")
	_ = sendCmd.MarkFlagRequired("to")
	sendCmd.Flags().String("subject", "", "message subject (required)")
	_ = sendCmd.MarkFlagRequired("subject")
	sendCmd.Flags().String("body", "", "message body")
	sendCmd.Flags().String("reply-to", "", "reply-to message ID")
	sendCmd.Flags().String("thread", "", "thread ID to attach to")
}

func runSend(cmd *cobra.Command, _ []string) error {
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetStringArray("to")
	subject, _ := cmd.Flags().GetString("subject")
	body, _ := cmd.Flags().GetString("body")
	replyTo, _ := cmd.Flags().GetString("reply-to")
	threadID, _ := cmd.Flags().GetString("thread")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	resp, err := client.SendMessage(ctx, &mailboxpb.SendMessageRequest{
		UserId:       from,
		RecipientIds: to,
		Subject:      subject,
		Body:         body,
		ReplyToId:    replyTo,
		ThreadId:     threadID,
	})
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}

	msg := resp.GetMessage()
	fmt.Printf("sent: id=%s status=%s\n", msg.GetId(), msg.GetStatus())
	return nil
}
