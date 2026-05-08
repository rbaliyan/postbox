package cmd

import (
	"fmt"
	"strings"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/emptypb"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage mailbox users (humans, agents, services)",
}

var userRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register a user (human, AI agent, or service)",
	RunE:  runUserRegister,
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered users",
	RunE:  runUserList,
}

var userSearchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search users by metadata (e.g. type=agent skills=web-search)",
	RunE:  runUserSearch,
}

var userGetCmd = &cobra.Command{
	Use:   "get <email>",
	Short: "Show the profile of a registered user",
	Args:  cobra.ExactArgs(1),
	RunE:  runUserGet,
}

func init() {
	userRegisterCmd.Flags().String("email", "", "user mailbox address (required)")
	userRegisterCmd.Flags().String("type", "", `principal type: "human", "agent", or "service"`)
	userRegisterCmd.Flags().String("public-key", "", "Ed25519 public key, base64-encoded (for agents)")
	userRegisterCmd.Flags().StringArray("meta", nil, "key=value metadata (repeatable, e.g. skills=web-search,image-gen endpoint=https://…)")
	_ = userRegisterCmd.MarkFlagRequired("email")

	userSearchCmd.Flags().StringArray("meta", nil, "key=value metadata filter (repeatable, substring-matched)")

	userCmd.AddCommand(userRegisterCmd, userListCmd, userSearchCmd, userGetCmd)
}

func runUserRegister(cmd *cobra.Command, _ []string) error {
	email, _ := cmd.Flags().GetString("email")
	userType, _ := cmd.Flags().GetString("type")
	pubKey, _ := cmd.Flags().GetString("public-key")
	metaStrs, _ := cmd.Flags().GetStringArray("meta")
	meta := parseMetaFlags(metaStrs)

	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email:     email,
		Type:      userType,
		PublicKey: pubKey,
		Metadata:  meta,
	})
	if err != nil {
		return fmt.Errorf("register user: %w", err)
	}
	fmt.Println(resp.GetMessage())
	return nil
}

func runUserList(cmd *cobra.Command, _ []string) error {
	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.ListUsers(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	if len(resp.GetUsers()) == 0 {
		fmt.Println("no users registered")
		return nil
	}
	for _, u := range resp.GetUsers() {
		printUser(u)
	}
	return nil
}

func runUserSearch(cmd *cobra.Command, _ []string) error {
	metaStrs, _ := cmd.Flags().GetStringArray("meta")
	filters := parseMetaFlags(metaStrs)

	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.SearchUsers(ctx, &postboxpb.SearchUsersRequest{
		MetadataFilters: filters,
	})
	if err != nil {
		return fmt.Errorf("search users: %w", err)
	}

	if len(resp.GetUsers()) == 0 {
		fmt.Println("no matching users")
		return nil
	}
	for _, u := range resp.GetUsers() {
		printUser(u)
	}
	return nil
}

func runUserGet(cmd *cobra.Command, args []string) error {
	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	u, err := client.GetUser(ctx, &postboxpb.GetUserRequest{Email: args[0]})
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	printUser(u)
	return nil
}

// printUser prints a formatted user profile to stdout.
func printUser(u *postboxpb.UserProfile) {
	fmt.Printf("email:    %s\n", u.GetEmail())
	if t := u.GetType(); t != "" {
		fmt.Printf("type:     %s\n", t)
	}
	if pk := u.GetPublicKey(); pk != "" {
		fmt.Printf("pubkey:   %s\n", pk)
	}
	if len(u.GetMetadata()) > 0 {
		pairs := make([]string, 0, len(u.GetMetadata()))
		for k, v := range u.GetMetadata() {
			pairs = append(pairs, k+"="+v)
		}
		// Sort for deterministic output.
		for i := 0; i < len(pairs)-1; i++ {
			for j := i + 1; j < len(pairs); j++ {
				if pairs[i] > pairs[j] {
					pairs[i], pairs[j] = pairs[j], pairs[i]
				}
			}
		}
		fmt.Printf("metadata: %s\n", strings.Join(pairs, " "))
	}
	fmt.Println()
}
