package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	mbxstore "github.com/rbaliyan/mailbox/store"

	"github.com/rbaliyan/postbox/internal/plugin"
)

// --- fake DraftMessage ---

type fakeDraft struct {
	senderID     string
	recipientIDs []string
	subject      string
	body         string
	headers      map[string]string
	attachments  []mbxstore.Attachment
}

func (d *fakeDraft) GetID() string                         { return "draft-id" }
func (d *fakeDraft) GetOwnerID() string                    { return "" }
func (d *fakeDraft) GetSenderID() string                   { return d.senderID }
func (d *fakeDraft) GetRecipientIDs() []string             { return d.recipientIDs }
func (d *fakeDraft) GetSubject() string                    { return d.subject }
func (d *fakeDraft) GetBody() string                       { return d.body }
func (d *fakeDraft) GetHeaders() map[string]string         { return d.headers }
func (d *fakeDraft) GetMetadata() map[string]any           { return nil }
func (d *fakeDraft) GetAttachments() []mbxstore.Attachment { return d.attachments }
func (d *fakeDraft) GetCreatedAt() time.Time               { return time.Time{} }
func (d *fakeDraft) GetUpdatedAt() time.Time               { return time.Time{} }
func (d *fakeDraft) GetExpiresAt() *time.Time              { return nil }
func (d *fakeDraft) SetSubject(s string) mbxstore.DraftMessage {
	d.subject = s
	return d
}
func (d *fakeDraft) SetBody(b string) mbxstore.DraftMessage {
	d.body = b
	return d
}
func (d *fakeDraft) SetRecipients(ids ...string) mbxstore.DraftMessage {
	d.recipientIDs = ids
	return d
}
func (d *fakeDraft) SetHeader(k, v string) mbxstore.DraftMessage {
	if d.headers == nil {
		d.headers = make(map[string]string)
	}
	d.headers[k] = v
	return d
}
func (d *fakeDraft) SetMetadata(k string, _ any) mbxstore.DraftMessage { return d }
func (d *fakeDraft) AddAttachment(a mbxstore.Attachment) mbxstore.DraftMessage {
	d.attachments = append(d.attachments, a)
	return d
}
func (d *fakeDraft) GetAvailableAt() *time.Time                      { return nil }
func (d *fakeDraft) SetTTL(_ time.Duration) mbxstore.DraftMessage    { return d }
func (d *fakeDraft) SetScheduleAt(_ time.Time) mbxstore.DraftMessage { return d }

// --- fake Attachment ---

type fakeAttachment struct {
	id          string
	filename    string
	contentType string
	size        int64
	uri         string
}

func (a fakeAttachment) GetID() string           { return a.id }
func (a fakeAttachment) GetFilename() string     { return a.filename }
func (a fakeAttachment) GetContentType() string  { return a.contentType }
func (a fakeAttachment) GetSize() int64          { return a.size }
func (a fakeAttachment) GetURI() string          { return a.uri }
func (a fakeAttachment) GetCreatedAt() time.Time { return time.Time{} }

// --- ErrRejected ---

func TestErrRejected_Sentinel(t *testing.T) {
	if plugin.ErrRejected == nil {
		t.Fatal("ErrRejected must not be nil")
	}
	if plugin.ErrRejected.Error() == "" {
		t.Fatal("ErrRejected must have a non-empty message")
	}
}

// ============================================================
// AddressFilter
// ============================================================

func TestAddressFilter_NameInitClose(t *testing.T) {
	f := plugin.NewAddressFilter("addr-filter", plugin.ModeBlock, nil)
	if f.Name() != "addr-filter" {
		t.Fatalf("Name()=%q", f.Name())
	}
	ctx := context.Background()
	if err := f.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := f.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAddressFilter_AfterSend_Noop(t *testing.T) {
	f := plugin.NewAddressFilter("addr-filter", plugin.ModeBlock, nil)
	if err := f.AfterSend(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestAddressFilter_NoRules_Allows(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	_ = f.Init(context.Background())
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"bob@example.com"}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
}

func TestAddressFilter_Block_MatchesSender(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `^blocked@`,
		Field:   plugin.FieldSender,
	})
	draft := &fakeDraft{senderID: "blocked@spam.com", recipientIDs: []string{"bob@example.com"}}
	err := f.BeforeSend(context.Background(), "", draft)
	if !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("expected ErrRejected, got: %v", err)
	}
}

func TestAddressFilter_Block_MatchesRecipient(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `^blocked@`,
		Field:   plugin.FieldRecipients,
	})
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"blocked@bad.com"}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection")
	}
}

func TestAddressFilter_Block_NoMatch_Allows(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `^blocked@`,
		Field:   plugin.FieldAll,
	})
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"bob@example.com"}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestAddressFilter_Allow_MatchAllows(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeAllow, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `@example\.com$`,
		Field:   plugin.FieldAll,
	})
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"bob@example.com"}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestAddressFilter_Allow_NoMatchRejects(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeAllow, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `@example\.com$`,
		Field:   plugin.FieldAll,
	})
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"outsider@other.com"}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for non-allowlisted recipient")
	}
}

func TestAddressFilter_Allow_SenderOnlyRule_DoesNotBlockRecipients(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeAllow, nil)
	_ = f.Init(context.Background())
	// Rule covers only FieldSender — recipients should pass through unchecked.
	_ = f.AddRule(context.Background(), plugin.AddressRule{
		ID:      "r1",
		Pattern: `@example\.com$`,
		Field:   plugin.FieldSender,
	})
	draft := &fakeDraft{
		senderID:     "alice@example.com",
		recipientIDs: []string{"bob@otherdomain.com"},
	}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("sender-only allowlist should not block recipients: %v", err)
	}
}

func TestAddressFilter_RemoveRule(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	_ = f.Init(context.Background())
	_ = f.AddRule(context.Background(), plugin.AddressRule{ID: "r1", Pattern: `^blocked@`, Field: plugin.FieldSender})
	_ = f.RemoveRule(context.Background(), "r1")
	draft := &fakeDraft{senderID: "blocked@spam.com", recipientIDs: []string{"bob@example.com"}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected nil after rule removal, got: %v", err)
	}
}

func TestAddressFilter_AddRule_InvalidPattern(t *testing.T) {
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, nil)
	err := f.AddRule(context.Background(), plugin.AddressRule{ID: "bad", Pattern: `[invalid`})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestAddressFilter_WithStore(t *testing.T) {
	ctx := context.Background()
	store := plugin.NewMemRuleStore()
	f := plugin.NewAddressFilter("f", plugin.ModeBlock, store)
	_ = f.Init(ctx)

	_ = f.AddRule(ctx, plugin.AddressRule{ID: "r1", Pattern: `^bad@`, Field: plugin.FieldSender})
	_ = f.Reload(ctx)

	draft := &fakeDraft{senderID: "bad@domain.com", recipientIDs: []string{"ok@example.com"}}
	if !errors.Is(f.BeforeSend(ctx, "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection from store-backed rule")
	}

	_ = f.RemoveRule(ctx, "r1")
	if err := f.BeforeSend(ctx, "", draft); err != nil {
		t.Fatalf("expected no rejection after removal: %v", err)
	}
}

func TestMemRuleStore_ListSaveDelete(t *testing.T) {
	ctx := context.Background()
	s := plugin.NewMemRuleStore(plugin.AddressRule{ID: "seed", Pattern: ".*", Field: plugin.FieldAll})

	rules, err := s.ListRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("expected 1 seeded rule, got %d, err=%v", len(rules), err)
	}

	_ = s.SaveRule(ctx, plugin.AddressRule{ID: "r2", Pattern: `@test`, Field: plugin.FieldSender})
	rules, _ = s.ListRules(ctx)
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(rules))
	}

	_ = s.DeleteRule(ctx, "r2")
	rules, _ = s.ListRules(ctx)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule after delete, got %d", len(rules))
	}
}

// ============================================================
// AttachmentFilter
// ============================================================

func TestAttachmentFilter_NameInitClose(t *testing.T) {
	f := plugin.NewAttachmentFilter("att-filter", plugin.AttachmentFilterConfig{})
	if f.Name() != "att-filter" {
		t.Fatalf("Name()=%q", f.Name())
	}
	ctx := context.Background()
	if err := f.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := f.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAttachmentFilter_AfterSend_Noop(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{})
	if err := f.AfterSend(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentFilter_NoAttachments_Passes(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{MaxCount: 1})
	draft := &fakeDraft{}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAttachmentFilter_MaxCount_Exceeded(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{MaxCount: 1})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "a.txt", contentType: "text/plain", size: 10},
		fakeAttachment{filename: "b.txt", contentType: "text/plain", size: 10},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for too many attachments")
	}
}

func TestAttachmentFilter_MaxSingleBytes_Exceeded(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{MaxSingleBytes: 100})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "big.bin", contentType: "application/octet-stream", size: 200},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for oversized attachment")
	}
}

func TestAttachmentFilter_MaxTotalBytes_Exceeded(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{MaxTotalBytes: 150})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "a.bin", contentType: "application/octet-stream", size: 100},
		fakeAttachment{filename: "b.bin", contentType: "application/octet-stream", size: 100},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for total size exceeded")
	}
}

func TestAttachmentFilter_BlockedMIME(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		BlockedMIMEs: []string{"application/x-executable"},
	})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "bad.exe", contentType: "application/x-executable", size: 10},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for blocked MIME")
	}
}

func TestAttachmentFilter_AllowedMIME_DisallowedType(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		AllowedMIMEs: []string{"image/png", "image/jpeg"},
	})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "doc.pdf", contentType: "application/pdf", size: 10},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for disallowed MIME")
	}
}

func TestAttachmentFilter_AllowedMIME_AllowedType(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		AllowedMIMEs: []string{"image/png"},
	})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "photo.png", contentType: "image/png", size: 10},
	}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestAttachmentFilter_MIMEWithParameters_Normalised(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		AllowedMIMEs: []string{"text/plain"},
	})
	// Content-Type includes charset parameter — should be stripped for comparison.
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "readme.txt", contentType: "text/plain; charset=utf-8", size: 5},
	}}
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection after MIME normalisation: %v", err)
	}
}

func TestAttachmentFilter_MalformedMIME_FallbackStrip(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		BlockedMIMEs: []string{"bad/type"},
	})
	// Malformed value that ParseMediaType rejects but raw strip succeeds.
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "x", contentType: "BAD/TYPE;unparseable==(", size: 5},
	}}
	// The fallback strips to "bad/type" which is in BlockedMIMEs.
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection via fallback MIME normalisation")
	}
}

func TestAttachmentFilter_NegativeSize_Rejected(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{MaxTotalBytes: 1000})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "bad.bin", contentType: "application/octet-stream", size: -1},
	}}
	if !errors.Is(f.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for negative attachment size")
	}
}

func TestAttachmentFilter_EmptyContentType(t *testing.T) {
	f := plugin.NewAttachmentFilter("f", plugin.AttachmentFilterConfig{
		BlockedMIMEs: []string{"text/plain"},
	})
	draft := &fakeDraft{attachments: []mbxstore.Attachment{
		fakeAttachment{filename: "x", contentType: "", size: 5},
	}}
	// Empty content type normalises to "" — not in blocked list, so passes.
	if err := f.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// ============================================================
// SpamChecker
// ============================================================

func TestSpamChecker_NameInitClose(t *testing.T) {
	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: "http://localhost"})
	if s.Name() != "spam" {
		t.Fatalf("Name()=%q", s.Name())
	}
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AfterSend(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSpamChecker_Rejects_WhenIsSpamTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"score": 8.5, "is_spam": true, "reason": "known spammer"}) //nolint:errcheck
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL, Threshold: 5.0})
	draft := &fakeDraft{senderID: "bad@spam.com", recipientIDs: []string{"bob@example.com"}, subject: "Buy now!"}
	if !errors.Is(s.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection")
	}
}

func TestSpamChecker_Rejects_WhenScoreExceedsThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"score": 9.0, "is_spam": false}) //nolint:errcheck
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL, Threshold: 7.0})
	draft := &fakeDraft{senderID: "alice@example.com", recipientIDs: []string{"bob@example.com"}}
	if !errors.Is(s.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection when score > threshold")
	}
}

func TestSpamChecker_Allows_BelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"score": 1.0, "is_spam": false}) //nolint:errcheck
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL, Threshold: 5.0})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := s.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSpamChecker_TagOnly_SetsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"score": 8.0, "is_spam": true, "reason": "bulk"}) //nolint:errcheck
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL, Threshold: 5.0, TagOnly: true})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := s.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected no rejection in TagOnly mode: %v", err)
	}
	if draft.headers["X-Spam-Status"] != "Yes" {
		t.Fatalf("expected X-Spam-Status=Yes, got %q", draft.headers["X-Spam-Status"])
	}
	if draft.headers["X-Spam-Score"] == "" {
		t.Fatal("expected X-Spam-Score to be set")
	}
}

func TestSpamChecker_EndpointUnreachable_Allows(t *testing.T) {
	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{
		Endpoint: "http://127.0.0.1:1",
		Timeout:  50 * time.Millisecond,
	})
	draft := &fakeDraft{senderID: "alice@example.com"}
	// Unreachable → skips check, allows message through.
	if err := s.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow on unreachable endpoint: %v", err)
	}
}

func TestSpamChecker_NonOKStatus_Allows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := s.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow on 500 response: %v", err)
	}
}

func TestSpamChecker_BadJSON_Allows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "not-json")
	}))
	defer srv.Close()

	s := plugin.NewSpamChecker("spam", plugin.SpamCheckerConfig{Endpoint: srv.URL})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := s.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow on bad JSON: %v", err)
	}
}

// ============================================================
// AntiVirus
// ============================================================

func TestAntiVirus_NameInitClose(t *testing.T) {
	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: "http://localhost"})
	if av.Name() != "av" {
		t.Fatalf("Name()=%q", av.Name())
	}
	ctx := context.Background()
	if err := av.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := av.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := av.AfterSend(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestAntiVirus_Clean_Passes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"clean": true}) //nolint:errcheck
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := av.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestAntiVirus_Threat_Rejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"clean": false,
			"threats": []map[string]any{
				{"attachment_id": "a1", "name": "Trojan.GenX", "description": "generic trojan"},
			},
		})
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{id: "a1", filename: "bad.exe", contentType: "application/octet-stream", size: 512, uri: "s3://bucket/bad.exe"},
		},
	}
	err := av.BeforeSend(context.Background(), "", draft)
	if !errors.Is(err, plugin.ErrRejected) {
		t.Fatalf("expected ErrRejected, got: %v", err)
	}
}

func TestAntiVirus_Unreachable_FailClose_Rejects(t *testing.T) {
	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{
		Endpoint: "http://127.0.0.1:1",
		Timeout:  50 * time.Millisecond,
		FailOpen: false,
	})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if !errors.Is(av.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection when fail-close and endpoint unreachable")
	}
}

func TestAntiVirus_Unreachable_FailOpen_Allows(t *testing.T) {
	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{
		Endpoint: "http://127.0.0.1:1",
		Timeout:  50 * time.Millisecond,
		FailOpen: true,
	})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := av.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow in fail-open mode: %v", err)
	}
}

func TestAntiVirus_NonOKStatus_FailClose_Rejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL, FailOpen: false})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if !errors.Is(av.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection on 503 with fail-close")
	}
}

func TestAntiVirus_NonOKStatus_FailOpen_Allows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL, FailOpen: true})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := av.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow on 503 with fail-open: %v", err)
	}
}

func TestAntiVirus_BadJSON_FailOpen_Allows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "not-json")
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL, FailOpen: true})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := av.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("expected allow on bad JSON with fail-open: %v", err)
	}
}

func TestAntiVirus_BadJSON_FailClose_Rejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "not-json")
	}))
	defer srv.Close()

	av := plugin.NewAntiVirus("av", plugin.AntiVirusConfig{Endpoint: srv.URL, FailOpen: false})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if !errors.Is(av.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection on bad JSON with fail-close")
	}
}

// ============================================================
// SecurityAgent
// ============================================================

func TestSecurityAgent_NameInitClose(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{SenderTTL: time.Second})
	if sa.Name() != "sec" {
		t.Fatalf("Name()=%q", sa.Name())
	}
	ctx := context.Background()
	if err := sa.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sa.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sa.AfterSend(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityAgent_CleanMessage_Passes(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{
		MaxRecipients: 5,
		MaxBodyBytes:  1000,
	})
	draft := &fakeDraft{
		senderID:     "alice@example.com",
		recipientIDs: []string{"bob@example.com"},
		body:         "Hello!",
		headers:      map[string]string{"X-Custom": "value"},
	}
	if err := sa.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSecurityAgent_TooManyRecipients(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{MaxRecipients: 2})
	draft := &fakeDraft{
		senderID:     "alice@example.com",
		recipientIDs: []string{"a@x.com", "b@x.com", "c@x.com"},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for too many recipients")
	}
}

func TestSecurityAgent_BodyTooLarge(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{MaxBodyBytes: 10})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		body:     "This body is definitely longer than ten bytes",
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for oversized body")
	}
}

func TestSecurityAgent_HeaderInjection_CR(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-Bad": "value\rinjected"},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for CR in header value")
	}
}

func TestSecurityAgent_HeaderInjection_LF(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-Bad\nX-Injected: evil": "value"},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for LF in header key")
	}
}

func TestSecurityAgent_BlockedExtension(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{filename: "payload.exe", contentType: "application/octet-stream", size: 1024},
		},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for .exe attachment")
	}
}

func TestSecurityAgent_DoubleExtensionBypass_Blocked(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	// Double-extension rename attack: "evil.exe.pdf" should still be caught.
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{filename: "evil.exe.pdf", contentType: "application/pdf", size: 512},
		},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for double-extension .exe.pdf file")
	}
}

func TestSecurityAgent_CustomBlockedExtension(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{
		BlockedExtensions: []string{".forbidden"},
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{filename: "secret.forbidden", contentType: "text/plain", size: 10},
		},
	}
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for custom blocked extension")
	}
}

func TestSecurityAgent_AllowedExtension_Passes(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{filename: "photo.png", contentType: "image/png", size: 1024},
		},
	}
	if err := sa.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection for .png: %v", err)
	}
}

func TestSecurityAgent_RateLimit(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{
		RatePerSender:  0.0001, // very slow refill
		BurstPerSender: 1,
		SenderTTL:      time.Minute,
	})
	draft := &fakeDraft{senderID: "rapid@example.com"}
	// First message consumes the burst.
	if err := sa.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("first message should pass: %v", err)
	}
	// Second message should be rate-limited.
	if !errors.Is(sa.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rate-limit rejection on second message")
	}
}

func TestSecurityAgent_NoExtension_Passes(t *testing.T) {
	sa := plugin.NewSecurityAgent("sec", plugin.SecurityAgentConfig{})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		attachments: []mbxstore.Attachment{
			fakeAttachment{filename: "README", contentType: "text/plain", size: 100},
		},
	}
	if err := sa.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection for no-extension file: %v", err)
	}
}
