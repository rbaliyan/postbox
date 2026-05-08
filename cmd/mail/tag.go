package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var tagCmd = &cobra.Command{
	Use:   "tag",
	Short: "Add or remove a tag on a message",
	RunE:  runTag,
}

func init() {
	tagCmd.Flags().String("user", "", "user email (required)")
	_ = tagCmd.MarkFlagRequired("user")
	tagCmd.Flags().String("id", "", "message ID (required)")
	_ = tagCmd.MarkFlagRequired("id")
	tagCmd.Flags().String("add", "", "tag to add")
	tagCmd.Flags().String("remove", "", "tag to remove")
}

func runTag(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	id, _ := cmd.Flags().GetString("id")
	add, _ := cmd.Flags().GetString("add")
	remove, _ := cmd.Flags().GetString("remove")

	if add == "" && remove == "" {
		return fmt.Errorf("provide --add or --remove")
	}

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	if add != "" {
		_, err = client.AddTag(ctx, &mailboxpb.TagRequest{
			UserId:    user,
			MessageId: id,
			TagId:     add,
		})
		if err != nil {
			return fmt.Errorf("add tag: %w", err)
		}
		fmt.Printf("tag %q added to message %s\n", add, id)
	}

	if remove != "" {
		_, err = client.RemoveTag(ctx, &mailboxpb.TagRequest{
			UserId:    user,
			MessageId: id,
			TagId:     remove,
		})
		if err != nil {
			return fmt.Errorf("remove tag: %w", err)
		}
		fmt.Printf("tag %q removed from message %s\n", remove, id)
	}

	return nil
}
