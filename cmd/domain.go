package cmd

import (
	"fmt"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/emptypb"
)

var domainCmd = &cobra.Command{
	Use:   "domain",
	Short: "Manage registered domains",
}

var domainListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all registered domains",
	RunE:  runDomainList,
}

var domainRemapCmd = &cobra.Command{
	Use:   "remap",
	Short: "Reassign a domain to a different node",
	RunE:  runDomainRemap,
}

func init() {
	domainRemapCmd.Flags().String("domain", "", "domain name to remap (required)")
	domainRemapCmd.Flags().String("node", "", "target node ID (required)")
	_ = domainRemapCmd.MarkFlagRequired("domain")
	_ = domainRemapCmd.MarkFlagRequired("node")

	domainCmd.AddCommand(domainListCmd, domainRemapCmd)
}

func runDomainList(cmd *cobra.Command, _ []string) error {
	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.ListDomains(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}

	if len(resp.GetDomains()) == 0 {
		fmt.Println("no domains registered")
		return nil
	}
	for _, d := range resp.GetDomains() {
		def := ""
		if d.GetIsDefault() {
			def = " (default)"
		}
		fmt.Printf("%-40s  node: %s%s\n", d.GetName(), d.GetNodeId(), def)
	}
	return nil
}

func runDomainRemap(cmd *cobra.Command, _ []string) error {
	domain, _ := cmd.Flags().GetString("domain")
	node, _ := cmd.Flags().GetString("node")

	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.RemapDomain(ctx, &postboxpb.RemapDomainRequest{
		Domain:       domain,
		TargetNodeId: node,
	})
	if err != nil {
		return fmt.Errorf("remap domain: %w", err)
	}
	fmt.Println(resp.GetMessage())
	return nil
}
