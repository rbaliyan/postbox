package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List messages in a folder",
	RunE:  runList,
}

func init() {
	listCmd.Flags().String("user", "", "user email (required)")
	_ = listCmd.MarkFlagRequired("user")
	listCmd.Flags().String("folder", "__inbox", "folder ID")
	listCmd.Flags().Int32("limit", 20, "max messages to return")
	listCmd.Flags().Int32("offset", 0, "pagination offset")
}

func runList(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")
	folder, _ := cmd.Flags().GetString("folder")
	limit, _ := cmd.Flags().GetInt32("limit")
	offset, _ := cmd.Flags().GetInt32("offset")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	resp, err := client.ListMessages(ctx, &mailboxpb.ListMessagesRequest{
		UserId:   user,
		FolderId: folder,
		Options:  &mailboxpb.ListOptions{Limit: limit, Offset: offset},
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	fmt.Printf("Folder: %s  User: %s  (total %d, hasMore: %v)\n\n",
		folder, user, resp.GetTotal(), resp.GetHasMore())
	for i, msg := range resp.GetMessages() {
		read := "[ ]"
		if msg.GetIsRead() {
			read = "[x]"
		}
		ts := msg.GetCreatedAt().AsTime().Format("2006-01-02 15:04")
		fmt.Printf("%3d. %s [%s] %s\n     from=%-30s  id=%s\n",
			i+1, read, ts, msg.GetSubject(), msg.GetSenderId(), msg.GetId())
	}
	if resp.GetHasMore() {
		fmt.Printf("\n(more results — use --offset %d)\n",
			int(offset)+len(resp.GetMessages()))
	}
	return nil
}
