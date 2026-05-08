package cmd

import (
	"context"
	"fmt"
	"time"

	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

var smtpCmd = &cobra.Command{
	Use:   "smtp",
	Short: "Manage the embedded SMTP listener",
}

var smtpStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the SMTP listener on the running server",
	RunE:  runSMTPStart,
}

var smtpStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the SMTP listener",
	RunE:  runSMTPStop,
}

var smtpStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show SMTP listener status",
	RunE:  runSMTPStatus,
}

func init() {
	smtpStartCmd.Flags().Int("port", 2525, "SMTP listen port")
	smtpStartCmd.Flags().String("domain", "localhost", "SMTP banner domain")
	smtpStartCmd.Flags().Bool("insecure-auth", false, "allow AUTH over plain-text connections (unsafe; enable only on private networks)")
	smtpStartCmd.Flags().Int64("max-msg-bytes", 0, "max message size in bytes (0 = 10 MiB default)")
	smtpStartCmd.Flags().Int("max-recipients", 0, "max recipients per message (0 = 50 default)")
	smtpStartCmd.Flags().Float64("rate-conns", 0, "max new connections per second (0 = unlimited)")
	smtpStartCmd.Flags().Int("rate-burst", 0, "connection rate burst capacity (0 = default 10)")

	smtpCmd.AddCommand(smtpStartCmd, smtpStopCmd, smtpStatusCmd)
}

func postboxClient(cmd *cobra.Command) (*grpc.ClientConn, postboxpb.PostboxServiceClient, error) {
	addr, _ := cmd.Root().PersistentFlags().GetString("addr")
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	return conn, postboxpb.NewPostboxServiceClient(conn), nil
}

func callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func runSMTPStart(cmd *cobra.Command, _ []string) error {
	port, _ := cmd.Flags().GetInt("port")
	domain, _ := cmd.Flags().GetString("domain")
	insecureAuth, _ := cmd.Flags().GetBool("insecure-auth")
	maxMsgBytes, _ := cmd.Flags().GetInt64("max-msg-bytes")
	maxRcpt, _ := cmd.Flags().GetInt("max-recipients")
	rateConns, _ := cmd.Flags().GetFloat64("rate-conns")
	rateBurst, _ := cmd.Flags().GetInt("rate-burst")

	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.StartSMTP(ctx, &postboxpb.SMTPConfig{
		Port:              int32(port),
		Domain:            domain,
		AllowInsecureAuth: insecureAuth,
		MaxMessageBytes:   maxMsgBytes,
		MaxRecipients:     int32(maxRcpt),
		MaxConnsPerSec:    rateConns,
		BurstConns:        int32(rateBurst),
	})
	if err != nil {
		return fmt.Errorf("start smtp: %w", err)
	}
	fmt.Println(resp.GetMessage())
	return nil
}

func runSMTPStop(cmd *cobra.Command, _ []string) error {
	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.StopSMTP(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("stop smtp: %w", err)
	}
	fmt.Println(resp.GetMessage())
	return nil
}

func runSMTPStatus(cmd *cobra.Command, _ []string) error {
	conn, client, err := postboxClient(cmd)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := callCtx()
	defer cancel()

	resp, err := client.GetSMTPStatus(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("get smtp status: %w", err)
	}

	running := "stopped"
	if resp.GetRunning() {
		running = "running"
	}
	fmt.Printf("smtp: %s  port: %d  domain: %s\n",
		running, resp.GetPort(), resp.GetDomain())
	return nil
}
