package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var foldersCmd = &cobra.Command{
	Use:   "folders",
	Short: "List all folders for a user",
	RunE:  runFolders,
}

func init() {
	foldersCmd.Flags().String("user", "", "user email (required)")
	_ = foldersCmd.MarkFlagRequired("user")
}

func runFolders(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	resp, err := client.ListFolders(ctx, &mailboxpb.ListFoldersRequest{UserId: user})
	if err != nil {
		return fmt.Errorf("list folders: %w", err)
	}

	fmt.Printf("Folders for %s:\n\n", user)
	fmt.Printf("%-20s %-8s %-8s %s\n", "ID", "Total", "Unread", "System")
	fmt.Printf("%-20s %-8s %-8s %s\n", "--------------------", "--------", "--------", "------")
	for _, f := range resp.GetFolders() {
		system := ""
		if f.GetIsSystem() {
			system = "yes"
		}
		fmt.Printf("%-20s %-8d %-8d %s\n",
			f.GetId(), f.GetMessageCount(), f.GetUnreadCount(), system)
	}
	return nil
}
