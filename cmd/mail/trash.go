package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var trashCmd = &cobra.Command{
	Use:   "trash",
	Short: "Move a message to trash",
	RunE:  runTrash,
}

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Restore a message from trash",
	RunE:  runRestore,
}

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Permanently delete a message",
	RunE:  runDelete,
}

func init() {
	for _, cmd := range []*cobra.Command{trashCmd, restoreCmd, deleteCmd} {
		cmd.Flags().String("user", "", "user email (required)")
		_ = cmd.MarkFlagRequired("user")
		cmd.Flags().String("id", "", "message ID (required)")
		_ = cmd.MarkFlagRequired("id")
	}
}

func runTrash(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	_, err = client.DeleteMessage(ctx, &mailboxpb.DeleteMessageRequest{UserId: user, MessageId: id})
	if err != nil {
		return fmt.Errorf("trash: %w", err)
	}
	fmt.Printf("message %s moved to trash\n", id)
	return nil
}

func runRestore(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	_, err = client.RestoreMessage(ctx, &mailboxpb.RestoreMessageRequest{UserId: user, MessageId: id})
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	fmt.Printf("message %s restored\n", id)
	return nil
}

func runDelete(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	_, err = client.PermanentlyDeleteMessage(ctx, &mailboxpb.PermanentlyDeleteMessageRequest{
		UserId:    user,
		MessageId: id,
	})
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	fmt.Printf("message %s permanently deleted\n", id)
	return nil
}
