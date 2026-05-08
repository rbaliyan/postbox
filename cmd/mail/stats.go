package mail

import (
	"fmt"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/spf13/cobra"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show mailbox statistics for a user",
	RunE:  runStats,
}

func init() {
	statsCmd.Flags().String("user", "", "user email (required)")
	_ = statsCmd.MarkFlagRequired("user")
}

func runStats(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetString("user")

	conn, client, err := dial(rootAddr(cmd))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := timeout()
	defer cancel()

	resp, err := client.GetStats(ctx, &mailboxpb.GetStatsRequest{UserId: user})
	if err != nil {
		return fmt.Errorf("get stats: %w", err)
	}

	s := resp.GetStats()
	fmt.Printf("Stats for %s:\n", user)
	fmt.Printf("  total:  %d\n", s.GetTotalMessages())
	fmt.Printf("  unread: %d\n", s.GetUnreadCount())
	fmt.Printf("  drafts: %d\n", s.GetDraftCount())
	if len(s.GetFolders()) > 0 {
		fmt.Println("\nPer-folder breakdown:")
		for folderID, counts := range s.GetFolders() {
			fmt.Printf("  %-20s  total=%-6d unread=%d\n",
				folderID, counts.GetTotal(), counts.GetUnread())
		}
	}
	return nil
}
