package webhook_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rbaliyan/event/v3"
	"github.com/rbaliyan/event/v3/transport/channel"
	"github.com/rbaliyan/mailbox"
	"github.com/rbaliyan/mailbox/notify"
	mailboxstore "github.com/rbaliyan/mailbox/store"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
	"github.com/rbaliyan/postbox/internal/webhook"
)

// ---------------------------------------------------------------------------
// Fakes (fakeMessage lives in payload_test.go in the same package)
// ---------------------------------------------------------------------------

// fakeUser implements mailbox.User.
type fakeUser struct {
	id           string
	capabilities map[string]string
}

func (u *fakeUser) ID() string                      { return u.id }
func (u *fakeUser) FirstName() string               { return "" }
func (u *fakeUser) LastName() string                { return "" }
func (u *fakeUser) Email() string                   { return u.id }
func (u *fakeUser) Type() string                    { return "agent" }
func (u *fakeUser) PublicKey() string               { return "" }
func (u *fakeUser) Capabilities() map[string]string { return u.capabilities }

// fakeUserResolver implements mailbox.UserResolver.
type fakeUserResolver struct {
	users map[string]*fakeUser
}

func newFakeUserResolver() *fakeUserResolver {
	return &fakeUserResolver{users: make(map[string]*fakeUser)}
}

func (r *fakeUserResolver) add(id, endpoint string) {
	r.users[id] = &fakeUser{id: id, capabilities: map[string]string{"endpoint": endpoint}}
}

func (r *fakeUserResolver) addNoEndpoint(id string) {
	r.users[id] = &fakeUser{id: id, capabilities: map[string]string{}}
}

func (r *fakeUserResolver) ResolveUser(_ context.Context, id string) (mailbox.User, error) {
	u, ok := r.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

// dispatcherMailbox implements mailbox.Mailbox for dispatcher tests.
// Only Get is exercised by the dispatcher; all other methods are no-ops.
type dispatcherMailbox struct {
	msg mailbox.Message
}

func (f *dispatcherMailbox) UserID() string { return "" }

// MessageClient
func (f *dispatcherMailbox) Get(_ context.Context, _ string) (mailbox.Message, error) {
	return f.msg, nil
}
func (f *dispatcherMailbox) Folder(_ context.Context, _ string, _ mailboxstore.ListOptions) (mailbox.MessageList, error) {
	return nil, nil
}
func (f *dispatcherMailbox) Search(_ context.Context, _ mailbox.SearchQuery) (mailbox.MessageList, error) {
	return nil, nil
}
func (f *dispatcherMailbox) Stream(_ context.Context, _ []mailboxstore.Filter, _ mailbox.StreamOptions) (mailbox.MessageIterator, error) {
	return nil, nil
}
func (f *dispatcherMailbox) StreamSearch(_ context.Context, _ mailbox.SearchQuery, _ mailbox.StreamOptions) (mailbox.MessageIterator, error) {
	return nil, nil
}
func (f *dispatcherMailbox) GetThread(_ context.Context, _ string, _ mailboxstore.ListOptions) (mailbox.MessageList, error) {
	return nil, nil
}
func (f *dispatcherMailbox) GetReplies(_ context.Context, _ string, _ mailboxstore.ListOptions) (mailbox.MessageList, error) {
	return nil, nil
}

// DraftClient
func (f *dispatcherMailbox) Drafts(_ context.Context, _ mailboxstore.ListOptions) (mailbox.DraftList, error) {
	return nil, nil
}
func (f *dispatcherMailbox) GetDraft(_ context.Context, _ string) (mailbox.Draft, error) {
	return nil, nil
}
func (f *dispatcherMailbox) Compose() (mailbox.Draft, error) { return nil, nil }

// StorageClient
func (f *dispatcherMailbox) ListFolders(_ context.Context) ([]mailbox.FolderInfo, error) {
	return nil, nil
}
func (f *dispatcherMailbox) LoadAttachment(_ context.Context, _, _ string) (io.ReadCloser, error) {
	return nil, nil
}

// MailboxMutator
func (f *dispatcherMailbox) UpdateFlags(_ context.Context, _ string, _ mailbox.Flags) error {
	return nil
}
func (f *dispatcherMailbox) MoveToFolder(_ context.Context, _ string, _ string, _ ...mailboxstore.MoveOption) error {
	return nil
}
func (f *dispatcherMailbox) Delete(_ context.Context, _ string) error              { return nil }
func (f *dispatcherMailbox) Restore(_ context.Context, _ string) error             { return nil }
func (f *dispatcherMailbox) PermanentlyDelete(_ context.Context, _ string) error   { return nil }
func (f *dispatcherMailbox) AddTag(_ context.Context, _ string, _ string) error    { return nil }
func (f *dispatcherMailbox) RemoveTag(_ context.Context, _ string, _ string) error { return nil }
func (f *dispatcherMailbox) MarkAllRead(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (f *dispatcherMailbox) UpdateByFilter(_ context.Context, _ []mailboxstore.Filter, _ mailbox.Flags) (int64, error) {
	return 0, nil
}
func (f *dispatcherMailbox) MoveByFilter(_ context.Context, _ []mailboxstore.Filter, _ string) (int64, error) {
	return 0, nil
}
func (f *dispatcherMailbox) DeleteByFilter(_ context.Context, _ []mailboxstore.Filter) (int64, error) {
	return 0, nil
}
func (f *dispatcherMailbox) TagByFilter(_ context.Context, _ []mailboxstore.Filter, _ string) (int64, error) {
	return 0, nil
}
func (f *dispatcherMailbox) UntagByFilter(_ context.Context, _ []mailboxstore.Filter, _ string) (int64, error) {
	return 0, nil
}

// MessageSender
func (f *dispatcherMailbox) SendMessage(_ context.Context, _ mailbox.SendRequest) (mailbox.Message, error) {
	return f.msg, nil
}

// BulkOperator
func (f *dispatcherMailbox) BulkUpdateFlags(_ context.Context, _ []string, _ mailbox.Flags) (*mailbox.BulkResult, error) {
	return nil, nil
}
func (f *dispatcherMailbox) BulkMove(_ context.Context, _ []string, _ string, _ ...mailboxstore.MoveOption) (*mailbox.BulkResult, error) {
	return nil, nil
}
func (f *dispatcherMailbox) BulkDelete(_ context.Context, _ []string) (*mailbox.BulkResult, error) {
	return nil, nil
}
func (f *dispatcherMailbox) BulkPermanentlyDelete(_ context.Context, _ []string) (*mailbox.BulkResult, error) {
	return nil, nil
}
func (f *dispatcherMailbox) BulkAddTag(_ context.Context, _ []string, _ string) (*mailbox.BulkResult, error) {
	return nil, nil
}
func (f *dispatcherMailbox) BulkRemoveTag(_ context.Context, _ []string, _ string) (*mailbox.BulkResult, error) {
	return nil, nil
}

// AttachmentResolver
func (f *dispatcherMailbox) ResolveAttachments(_ context.Context, _ []string) ([]mailboxstore.Attachment, error) {
	return nil, nil
}

// StatsReader
func (f *dispatcherMailbox) Stats(_ context.Context) (*mailboxstore.MailboxStats, error) {
	return nil, nil
}
func (f *dispatcherMailbox) UnreadCount(_ context.Context) (int64, error) { return 0, nil }

// dispatcherMailboxService implements mailbox.Service with a real event bus.
type dispatcherMailboxService struct {
	events *mailbox.ServiceEvents
	mb     *dispatcherMailbox
}

func newDispatcherMailboxService(msg mailbox.Message) (*dispatcherMailboxService, error) {
	mb := &dispatcherMailbox{msg: msg}
	bus, err := event.NewBus(
		"disp-test-"+time.Now().Format("150405.000000000"),
		event.WithTransport(channel.New()),
	)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	ev := event.New[mailbox.MessageReceivedEvent]("disp.test.mailbox.message.received." + time.Now().Format("150405.000000000"))
	if err := event.Register(ctx, bus, ev); err != nil {
		return nil, err
	}
	events := &mailbox.ServiceEvents{
		MessageReceived: ev,
	}
	return &dispatcherMailboxService{events: events, mb: mb}, nil
}

func (s *dispatcherMailboxService) IsConnected() bool               { return true }
func (s *dispatcherMailboxService) Connect(_ context.Context) error { return nil }
func (s *dispatcherMailboxService) Close(_ context.Context) error   { return nil }
func (s *dispatcherMailboxService) Client(_ string) mailbox.Mailbox { return s.mb }
func (s *dispatcherMailboxService) MailboxID() string               { return "test" }
func (s *dispatcherMailboxService) Events() *mailbox.ServiceEvents  { return s.events }
func (s *dispatcherMailboxService) Notifications(_ context.Context, _, _ string) (notify.Stream, error) {
	return nil, nil
}
func (s *dispatcherMailboxService) CleanupTrash(_ context.Context) (*mailbox.CleanupTrashResult, error) {
	return &mailbox.CleanupTrashResult{}, nil
}
func (s *dispatcherMailboxService) CleanupExpiredMessages(_ context.Context) (*mailbox.CleanupExpiredMessagesResult, error) {
	return &mailbox.CleanupExpiredMessagesResult{}, nil
}
func (s *dispatcherMailboxService) EnforceQuotas(_ context.Context, _ []string) (*mailbox.EnforceQuotasResult, error) {
	return &mailbox.EnforceQuotasResult{}, nil
}
func (s *dispatcherMailboxService) RunQuotaEnforcement(_ context.Context) (*mailbox.EnforceQuotasResult, error) {
	return &mailbox.EnforceQuotasResult{}, nil
}
func (s *dispatcherMailboxService) ThreadParticipants(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestSigner(t *testing.T) *webhook.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := webhook.NewSigner(priv, "v1")
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return s
}

func makeDispatcher(
	t *testing.T,
	resolver mailbox.UserResolver,
	jobs *memstore.Store,
	svc mailbox.Service,
	opts ...webhook.Option,
) *webhook.Dispatcher {
	t.Helper()
	signer := newTestSigner(t)
	defaults := []webhook.Option{
		webhook.WithWorkers(1),
		webhook.WithSweepPeriod(1 * time.Hour),
	}
	all := append(defaults, opts...)
	return webhook.New(resolver, jobs, svc, signer, all...)
}

// waitForJobStatus polls the job store until the job reaches wantStatus or the timeout expires.
func waitForJobStatus(
	t *testing.T,
	jobs *memstore.Store,
	messageID, recipientID string,
	wantStatus store.DeliveryStatus,
	timeout time.Duration,
) store.DeliveryJob {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := jobs.GetDeliveryJob(ctx, messageID, recipientID)
		if err == nil && job.Status == wantStatus {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := jobs.GetDeliveryJob(ctx, messageID, recipientID)
	return job
}

// publishEvent fires a MessageReceivedEvent on the service's event bus.
func publishEvent(t *testing.T, svc *dispatcherMailboxService, ev mailbox.MessageReceivedEvent) {
	t.Helper()
	if err := svc.events.MessageReceived.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish event: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDispatcher_DeliverSuccess(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resolver := newFakeUserResolver()
	resolver.add("agent@example.com", srv.URL)

	msg := &fakeMessage{id: "msg-1", senderID: "sender@example.com", subject: "Hello"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc,
		webhook.WithHTTPClient(srv.Client()),
	)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(d.Stop)

	ev := mailbox.MessageReceivedEvent{
		MessageID:   "msg-1",
		RecipientID: "agent@example.com",
		SenderID:    "sender@example.com",
		Subject:     "Hello",
		ReceivedAt:  time.Now(),
	}
	publishEvent(t, svc, ev)
	job := waitForJobStatus(t, jobs, ev.MessageID, ev.RecipientID, store.DeliveryDelivered, 5*time.Second)

	if job.Status != store.DeliveryDelivered {
		t.Fatalf("expected delivered, got %q (last_error=%s)", job.Status, job.LastError)
	}
	if called.Load() == 0 {
		t.Fatal("expected HTTP server to be called")
	}
	if job.DeliveredAt == nil {
		t.Fatal("expected DeliveredAt to be set")
	}
}

func TestDispatcher_DeliverFails_Retry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500 — transient, should retry
	}))
	defer srv.Close()

	resolver := newFakeUserResolver()
	resolver.add("agent@example.com", srv.URL)

	msg := &fakeMessage{id: "msg-2", senderID: "sender@example.com", subject: "Test"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc,
		webhook.WithHTTPClient(srv.Client()),
		webhook.WithMaxAttempts(3),
	)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(d.Stop)

	ev := mailbox.MessageReceivedEvent{
		MessageID:   "msg-2",
		RecipientID: "agent@example.com",
		SenderID:    "sender@example.com",
		Subject:     "Test",
		ReceivedAt:  time.Now(),
	}
	publishEvent(t, svc, ev)
	// After the first failure, the job should be in failed or dead state.
	job := waitForJobStatus(t, jobs, ev.MessageID, ev.RecipientID, store.DeliveryFailed, 3*time.Second)
	if job.Status != store.DeliveryFailed && job.Status != store.DeliveryDead {
		t.Fatalf("expected failed or dead, got %q", job.Status)
	}
	if job.Attempts == 0 {
		t.Fatal("expected at least one attempt to be recorded")
	}
	if job.LastError == "" {
		t.Fatal("expected LastError to describe the HTTP failure")
	}
}

func TestDispatcher_DeliverFails_Permanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 400 — permanent failure, no retry
	}))
	defer srv.Close()

	resolver := newFakeUserResolver()
	resolver.add("agent@example.com", srv.URL)

	msg := &fakeMessage{id: "msg-3", senderID: "sender@example.com", subject: "Test"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc,
		webhook.WithHTTPClient(srv.Client()),
		webhook.WithMaxAttempts(5),
	)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(d.Stop)

	ev := mailbox.MessageReceivedEvent{
		MessageID:   "msg-3",
		RecipientID: "agent@example.com",
		SenderID:    "sender@example.com",
		Subject:     "Test",
		ReceivedAt:  time.Now(),
	}
	publishEvent(t, svc, ev)
	job := waitForJobStatus(t, jobs, ev.MessageID, ev.RecipientID, store.DeliveryDead, 3*time.Second)

	if job.Status != store.DeliveryDead {
		t.Fatalf("expected dead (permanent failure on 400), got %q", job.Status)
	}
	// isPermanentFailure(400) == true, so it should die on the very first attempt.
	if job.Attempts != 1 {
		t.Fatalf("expected exactly 1 attempt for a permanent 400 failure, got %d", job.Attempts)
	}
}

func TestDispatcher_NoEndpoint(t *testing.T) {
	resolver := newFakeUserResolver()
	resolver.addNoEndpoint("agent@example.com") // registered but no endpoint URL

	msg := &fakeMessage{id: "msg-4", senderID: "sender@example.com", subject: "Test"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(d.Stop)

	ev := mailbox.MessageReceivedEvent{
		MessageID:   "msg-4",
		RecipientID: "agent@example.com",
		SenderID:    "sender@example.com",
		Subject:     "Test",
		ReceivedAt:  time.Now(),
	}
	publishEvent(t, svc, ev)

	// Give the dispatcher time to process the event (should be a no-op).
	time.Sleep(200 * time.Millisecond)

	_, err = jobs.GetDeliveryJob(ctx, ev.MessageID, ev.RecipientID)
	if err == nil {
		t.Fatal("expected no delivery job to be created for a user with no endpoint")
	}
}

func TestDispatcher_UnknownUser_NoJob(t *testing.T) {
	resolver := newFakeUserResolver() // empty resolver — user is unknown

	msg := &fakeMessage{id: "msg-5"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(d.Stop)

	ev := mailbox.MessageReceivedEvent{
		MessageID:   "msg-5",
		RecipientID: "unknown@example.com",
		ReceivedAt:  time.Now(),
	}
	publishEvent(t, svc, ev)
	time.Sleep(200 * time.Millisecond)

	_, err = jobs.GetDeliveryJob(ctx, ev.MessageID, ev.RecipientID)
	if err == nil {
		t.Fatal("expected no delivery job for an unknown user")
	}
}

func TestDispatcher_Stop(t *testing.T) {
	resolver := newFakeUserResolver()
	msg := &fakeMessage{id: "msg-stop"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	d := makeDispatcher(t, resolver, jobs, svc)

	ctx := context.Background()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		d.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop returned without hanging.
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return within the 3 s timeout")
	}
}

func TestDispatcher_New_ReturnsNonNil(t *testing.T) {
	resolver := newFakeUserResolver()
	msg := &fakeMessage{id: "msg-new"}
	svc, err := newDispatcherMailboxService(msg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	jobs := memstore.New()
	signer := newTestSigner(t)
	d := webhook.New(resolver, jobs, svc, signer)
	if d == nil {
		t.Fatal("New returned nil")
	}
}
