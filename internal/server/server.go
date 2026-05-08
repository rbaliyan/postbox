package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rbaliyan/postbox/internal/registry"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/store"
	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

var _ postboxpb.PostboxServiceServer = (*PostboxServer)(nil)

// PostboxServer implements the PostboxService gRPC interface.
//
// It is a thin adapter over a Controller: every RPC translates the gRPC
// request into one or more calls on the controller's store, registry, or
// SMTP lifecycle.
type PostboxServer struct {
	postboxpb.UnimplementedPostboxServiceServer
	ctrl *Controller
}

// NewServer wraps a Controller in a gRPC PostboxService implementation.
func NewServer(ctrl *Controller) *PostboxServer {
	return &PostboxServer{ctrl: ctrl}
}

// Discover returns the node responsible for the given domain or user email.
// Returns InvalidArgument if target is empty, NotFound if no mapping exists.
func (s *PostboxServer) Discover(ctx context.Context, req *postboxpb.DiscoverRequest) (*postboxpb.NodeInfo, error) {
	target := req.GetTarget()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target is required")
	}
	nodeID, err := s.ctrl.registry.Lookup(ctx, target)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) || errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "no node for %q", target)
		}
		return nil, status.Errorf(codes.Internal, "lookup: %v", err)
	}
	// Resolve the owning node's address. For the local node this is always
	// ctrl.address; for remote nodes use NodeResolver (Redis-backed registry).
	address := s.ctrl.address
	if nodeID != s.ctrl.nodeID {
		if resolver, ok := s.ctrl.registry.(registry.NodeResolver); ok {
			if addr, err := resolver.ResolveNode(ctx, nodeID); err == nil {
				address = addr
			}
		}
	}
	return &postboxpb.NodeInfo{
		NodeId:  nodeID,
		Address: address,
		Mode:    string(s.ctrl.mode),
	}, nil
}

// RegisterDomain claims a domain name for the local node.
// Returns AlreadyExists if the domain is owned by a different node, and is
// idempotent if it is already owned by this node.
func (s *PostboxServer) RegisterDomain(ctx context.Context, req *postboxpb.RegisterDomainRequest) (*postboxpb.Response, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}

	existing, err := s.ctrl.store.GetDomain(ctx, name)
	switch {
	case err == nil:
		if existing.NodeID != s.ctrl.nodeID {
			return nil, status.Errorf(codes.AlreadyExists,
				"domain %q already registered to node %s", name, existing.NodeID)
		}
		return &postboxpb.Response{
			Ok:      true,
			Message: fmt.Sprintf("domain %q already registered to node %s", name, existing.NodeID),
		}, nil
	case errors.Is(err, store.ErrNotFound):
		// fall through to register
	default:
		return nil, status.Errorf(codes.Internal, "check domain: %v", err)
	}

	if err := s.ctrl.registry.Register(ctx, registry.Entity{
		Type:      registry.EntityDomain,
		Name:      name,
		IsDefault: req.GetIsDefault(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "register domain: %v", err)
	}
	return &postboxpb.Response{
		Ok:      true,
		Message: fmt.Sprintf("domain %q registered to node %s", name, s.ctrl.nodeID),
	}, nil
}

// RegisterUser registers a mailbox principal — human, AI agent, or service.
// Idempotent: returns ok=true if the user already exists.
func (s *PostboxServer) RegisterUser(ctx context.Context, req *postboxpb.RegisterUserRequest) (*postboxpb.Response, error) {
	email := req.GetEmail()
	if email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}

	existing, err := s.ctrl.store.GetUser(ctx, email)
	switch {
	case err == nil:
		// Update type, public_key, and metadata on re-registration.
		existing.Type = req.GetType()
		existing.PublicKeyB64 = req.GetPublicKey()
		existing.Metadata = req.GetMetadata()
		if err := s.ctrl.store.SaveUser(ctx, existing); err != nil {
			return nil, status.Errorf(codes.Internal, "update user: %v", err)
		}
		return &postboxpb.Response{
			Ok: true,
			Message: fmt.Sprintf("user %q updated (created %s)",
				email, existing.CreatedAt.Format(time.RFC3339)),
		}, nil
	case errors.Is(err, store.ErrNotFound):
		// fall through to register
	default:
		return nil, status.Errorf(codes.Internal, "check user: %v", err)
	}

	if err := s.ctrl.registry.Register(ctx, registry.Entity{
		Type:     registry.EntityUser,
		Name:     email,
		UserType: req.GetType(),
		Metadata: req.GetMetadata(),
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "register user: %v", err)
	}
	// Persist the public_key — the registry only stores type and metadata.
	if pk := req.GetPublicKey(); pk != "" {
		u, _ := s.ctrl.store.GetUser(ctx, email)
		u.PublicKeyB64 = pk
		if err := s.ctrl.store.SaveUser(ctx, u); err != nil {
			return nil, status.Errorf(codes.Internal, "save public key: %v", err)
		}
	}
	return &postboxpb.Response{
		Ok:      true,
		Message: fmt.Sprintf("user %q registered to node %s", email, s.ctrl.nodeID),
	}, nil
}

// GetUser retrieves a user profile by email address.
func (s *PostboxServer) GetUser(ctx context.Context, req *postboxpb.GetUserRequest) (*postboxpb.UserProfile, error) {
	email := req.GetEmail()
	if email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}
	u, err := s.ctrl.store.GetUser(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "user %q not found", email)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get user: %v", err)
	}
	return userToProto(u, s.ctrl.nodeID), nil
}

// SearchUsers returns users whose metadata matches all supplied filters.
func (s *PostboxServer) SearchUsers(ctx context.Context, req *postboxpb.SearchUsersRequest) (*postboxpb.SearchUsersResponse, error) {
	users, err := s.ctrl.store.SearchUsers(ctx, req.GetMetadataFilters())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search users: %v", err)
	}
	out := make([]*postboxpb.UserProfile, 0, len(users))
	for _, u := range users {
		out = append(out, userToProto(u, s.ctrl.nodeID))
	}
	return &postboxpb.SearchUsersResponse{Users: out}, nil
}

// ListUsers returns all registered users.
func (s *PostboxServer) ListUsers(ctx context.Context, _ *emptypb.Empty) (*postboxpb.ListUsersResponse, error) {
	users, err := s.ctrl.store.ListUsers(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list users: %v", err)
	}
	out := make([]*postboxpb.UserProfile, 0, len(users))
	for _, u := range users {
		out = append(out, userToProto(u, s.ctrl.nodeID))
	}
	return &postboxpb.ListUsersResponse{Users: out}, nil
}

// GetNodePublicKey returns the node's Ed25519 public key for webhook signature verification.
func (s *PostboxServer) GetNodePublicKey(_ context.Context, _ *emptypb.Empty) (*postboxpb.NodePublicKeyResponse, error) {
	return &postboxpb.NodePublicKeyResponse{
		PublicKey: s.ctrl.nodePubKeyB64,
		NodeId:    s.ctrl.nodeID,
		KeyId:     "v1",
	}, nil
}

// GetStatus returns health and capacity metrics for the local node.
func (s *PostboxServer) GetStatus(ctx context.Context, _ *emptypb.Empty) (*postboxpb.StatusReport, error) {
	domainCount, err := s.ctrl.store.CountDomains(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count domains: %v", err)
	}
	userCount, err := s.ctrl.store.CountUsers(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count users: %v", err)
	}
	return &postboxpb.StatusReport{
		NodeId:           s.ctrl.nodeID,
		Mode:             string(s.ctrl.mode),
		DomainCount:      domainCount,
		UserCount:        userCount,
		MailboxConnected: s.ctrl.mailbox.IsConnected(),
	}, nil
}

// RemapDomain reassigns a domain to a different node (admin operation).
func (s *PostboxServer) RemapDomain(ctx context.Context, req *postboxpb.RemapDomainRequest) (*postboxpb.Response, error) {
	name := req.GetDomain()
	target := req.GetTargetNodeId()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "domain is required")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target_node_id is required")
	}
	d, err := s.ctrl.store.GetDomain(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "domain %q not found", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get domain: %v", err)
	}
	d.NodeID = target
	if err := s.ctrl.store.SaveDomain(ctx, d); err != nil {
		return nil, status.Errorf(codes.Internal, "save domain: %v", err)
	}
	return &postboxpb.Response{
		Ok:      true,
		Message: fmt.Sprintf("domain %q remapped to %s", name, target),
	}, nil
}

// ListDomains returns all registered domains.
func (s *PostboxServer) ListDomains(ctx context.Context, _ *emptypb.Empty) (*postboxpb.ListDomainsResponse, error) {
	domains, err := s.ctrl.store.ListDomains(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list domains: %v", err)
	}
	out := make([]*postboxpb.DomainInfo, 0, len(domains))
	for _, d := range domains {
		out = append(out, &postboxpb.DomainInfo{
			Name:      d.Name,
			NodeId:    d.NodeID,
			IsDefault: d.IsDefault,
		})
	}
	return &postboxpb.ListDomainsResponse{Domains: out}, nil
}

// StartSMTP starts the embedded SMTP listener with the provided configuration.
func (s *PostboxServer) StartSMTP(_ context.Context, req *postboxpb.SMTPConfig) (*postboxpb.Response, error) {
	cfg := smtpConfigFromProto(req)
	if err := s.ctrl.StartSMTP(cfg); err != nil {
		if errors.Is(err, ErrSMTPAlreadyRunning) {
			return nil, status.Errorf(codes.FailedPrecondition, "start smtp: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "start smtp: %v", err)
	}
	srv := s.ctrl.smtpServer()
	return &postboxpb.Response{
		Ok:      true,
		Message: fmt.Sprintf("SMTP listener started on :%d (domain %s)", srv.Port(), srv.Domain()),
	}, nil
}

// StopSMTP stops the running SMTP listener.
// Returns FailedPrecondition if the listener is not currently running.
func (s *PostboxServer) StopSMTP(_ context.Context, _ *emptypb.Empty) (*postboxpb.Response, error) {
	if err := s.ctrl.StopSMTP(); err != nil {
		if errors.Is(err, ErrSMTPNotRunning) {
			return nil, status.Errorf(codes.FailedPrecondition, "stop smtp: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "stop smtp: %v", err)
	}
	return &postboxpb.Response{Ok: true, Message: "SMTP listener stopped"}, nil
}

// GetSMTPStatus returns the current state of the embedded SMTP listener.
func (s *PostboxServer) GetSMTPStatus(_ context.Context, _ *emptypb.Empty) (*postboxpb.SMTPStatus, error) {
	srv := s.ctrl.smtpServer()
	if srv == nil {
		return &postboxpb.SMTPStatus{Running: false}, nil
	}
	return &postboxpb.SMTPStatus{
		Running: srv.IsRunning(),
		Port:    int32(srv.Port()),
		Domain:  srv.Domain(),
	}, nil
}

// GetDeliveryStatus returns webhook delivery attempts for a message/recipient pair.
func (s *PostboxServer) GetDeliveryStatus(ctx context.Context, req *postboxpb.GetDeliveryStatusRequest) (*postboxpb.DeliveryStatusResponse, error) {
	msgID := req.GetMessageId()
	recipID := req.GetRecipientId()
	if msgID == "" {
		return nil, status.Error(codes.InvalidArgument, "message_id is required")
	}
	if recipID == "" {
		return nil, status.Error(codes.InvalidArgument, "recipient_id is required")
	}
	job, err := s.ctrl.store.GetDeliveryJob(ctx, msgID, recipID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no delivery job for message %q recipient %q", msgID, recipID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get delivery job: %v", err)
	}
	return deliveryJobToProto(job), nil
}

// ListDeliveryFailures returns failed and dead delivery jobs, optionally filtered by recipient.
func (s *PostboxServer) ListDeliveryFailures(ctx context.Context, req *postboxpb.ListDeliveryFailuresRequest) (*postboxpb.ListDeliveryFailuresResponse, error) {
	recipID := req.GetRecipientId()
	failed, err := s.ctrl.store.ListDeliveryJobsByRecipient(ctx, recipID, store.DeliveryFailed)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list failed jobs: %v", err)
	}
	dead, err := s.ctrl.store.ListDeliveryJobsByRecipient(ctx, recipID, store.DeliveryDead)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list dead jobs: %v", err)
	}
	jobs := append(failed, dead...)
	out := make([]*postboxpb.DeliveryStatusResponse, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, deliveryJobToProto(j))
	}
	return &postboxpb.ListDeliveryFailuresResponse{Jobs: out}, nil
}

// userToProto converts a store User to the proto representation.
func userToProto(u store.User, nodeID string) *postboxpb.UserProfile {
	return &postboxpb.UserProfile{
		Email:     u.Email,
		Type:      u.Type,
		PublicKey: u.PublicKeyB64,
		Metadata:  u.Metadata,
		NodeId:    nodeID,
	}
}

// deliveryJobToProto converts a store.DeliveryJob to the proto representation.
func deliveryJobToProto(j store.DeliveryJob) *postboxpb.DeliveryStatusResponse {
	r := &postboxpb.DeliveryStatusResponse{
		MessageId:   j.MessageID,
		RecipientId: j.RecipientID,
		EndpointUrl: j.EndpointURL,
		Status:      string(j.Status),
		Attempts:    int32(j.Attempts),
		MaxAttempts: int32(j.MaxAttempts),
		LastError:   j.LastError,
	}
	if !j.NextRetryAt.IsZero() {
		r.NextRetryAt = j.NextRetryAt.Format(time.RFC3339)
	}
	if j.DeliveredAt != nil {
		r.DeliveredAt = j.DeliveredAt.Format(time.RFC3339)
	}
	return r
}

// smtpConfigFromProto converts the protobuf SMTPConfig into the internal
// postboxsmtp.Config. Defaults are applied later in postboxsmtp.New.
func smtpConfigFromProto(req *postboxpb.SMTPConfig) postboxsmtp.Config {
	return postboxsmtp.Config{
		Port:              int(req.GetPort()),
		Domain:            req.GetDomain(),
		AllowInsecureAuth: req.GetAllowInsecureAuth(),
		MaxMessageBytes:   req.GetMaxMessageBytes(),
		MaxRecipients:     int(req.GetMaxRecipients()),
		ReadTimeout:       time.Duration(req.GetReadTimeoutSecs()) * time.Second,
		WriteTimeout:      time.Duration(req.GetWriteTimeoutSecs()) * time.Second,
		MaxConnsPerSec:    req.GetMaxConnsPerSec(),
		BurstConns:        int(req.GetBurstConns()),
	}
}
