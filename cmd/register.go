package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var registerCmd = &cobra.Command{
	Use:   "register",
	Short: "Register a domain or user with the local node",
	RunE:  runRegister,
}

func init() {
	registerCmd.Flags().String("domain", "", "domain name to register (e.g. example.com)")
	registerCmd.Flags().Bool("default", false, "mark this domain as the default fallback")
	registerCmd.Flags().String("user", "", "user email to register (e.g. alice@example.com)")
	registerCmd.Flags().StringArray("meta", nil, "key=value metadata for a user registration")
}

func runRegister(cmd *cobra.Command, _ []string) error {
	addr, _ := cmd.Root().PersistentFlags().GetString("addr")
	domainName, _ := cmd.Flags().GetString("domain")
	isDefault, _ := cmd.Flags().GetBool("default")
	userEmail, _ := cmd.Flags().GetString("user")
	metaSlice, _ := cmd.Flags().GetStringArray("meta")

	if domainName == "" && userEmail == "" {
		return fmt.Errorf("provide --domain or --user")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close() //nolint:errcheck

	client := postboxpb.NewPostboxServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if domainName != "" {
		resp, err := client.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{
			Name:      domainName,
			IsDefault: isDefault,
		})
		if err != nil {
			return fmt.Errorf("register domain: %w", err)
		}
		fmt.Println(resp.GetMessage())
		return nil
	}

	meta := parseMetaFlags(metaSlice)
	resp, err := client.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email:    userEmail,
		Metadata: meta,
	})
	if err != nil {
		return fmt.Errorf("register user: %w", err)
	}
	fmt.Println(resp.GetMessage())
	return nil
}

func parseMetaFlags(pairs []string) map[string]string {
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		if k != "" {
			m[k] = v
		}
	}
	return m
}
