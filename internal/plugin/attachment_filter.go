package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"strings"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// AttachmentFilterConfig drives the AttachmentFilter checks.
// Zero values mean "no limit / no restriction".
type AttachmentFilterConfig struct {
	// MaxCount is the maximum number of attachments allowed per message.
	// 0 means unlimited.
	MaxCount int
	// MaxTotalBytes is the maximum cumulative attachment size in bytes.
	// 0 means unlimited.
	MaxTotalBytes int64
	// MaxSingleBytes is the maximum size of any individual attachment.
	// 0 means unlimited.
	MaxSingleBytes int64
	// AllowedMIMEs is an exclusive allowlist of MIME media types (without
	// parameters, e.g. "image/png"). When non-empty, attachments whose
	// media type is not in this set are rejected.
	AllowedMIMEs []string
	// BlockedMIMEs is an additive blocklist of MIME media types. Any
	// attachment whose media type appears here is rejected regardless of
	// AllowedMIMEs.
	BlockedMIMEs []string
}

// AttachmentFilter is a mailbox.SendHook that enforces attachment policies
// before a message is stored.
type AttachmentFilter struct {
	name    string
	cfg     AttachmentFilterConfig
	allowed map[string]struct{} // pre-built for O(1) lookup
	blocked map[string]struct{}
	logger  *slog.Logger
}

var _ mailbox.Plugin = (*AttachmentFilter)(nil)
var _ mailbox.SendHook = (*AttachmentFilter)(nil)

// AttachmentFilterOption configures an AttachmentFilter.
type AttachmentFilterOption func(*AttachmentFilter)

// WithAttachmentFilterLogger sets the structured logger.
func WithAttachmentFilterLogger(l *slog.Logger) AttachmentFilterOption {
	return func(f *AttachmentFilter) { f.logger = l }
}

// NewAttachmentFilter creates an AttachmentFilter plugin.
func NewAttachmentFilter(name string, cfg AttachmentFilterConfig, opts ...AttachmentFilterOption) *AttachmentFilter {
	f := &AttachmentFilter{
		name:    name,
		cfg:     cfg,
		allowed: toSet(cfg.AllowedMIMEs),
		blocked: toSet(cfg.BlockedMIMEs),
		logger:  slog.Default(),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func (f *AttachmentFilter) Name() string                  { return f.name }
func (f *AttachmentFilter) Init(_ context.Context) error  { return nil }
func (f *AttachmentFilter) Close(_ context.Context) error { return nil }
func (f *AttachmentFilter) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend enforces count, size, and MIME type constraints.
func (f *AttachmentFilter) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	atts := draft.GetAttachments()

	if f.cfg.MaxCount > 0 && len(atts) > f.cfg.MaxCount {
		return reject(f.name, fmt.Sprintf(
			"too many attachments: %d > max %d", len(atts), f.cfg.MaxCount))
	}

	var totalBytes int64
	for _, a := range atts {
		sz := a.GetSize()
		if sz < 0 {
			return reject(f.name, fmt.Sprintf(
				"attachment %q has invalid negative size %d", a.GetFilename(), sz))
		}
		totalBytes += sz

		if f.cfg.MaxSingleBytes > 0 && sz > f.cfg.MaxSingleBytes {
			return reject(f.name, fmt.Sprintf(
				"attachment %q size %d exceeds limit of %d bytes",
				a.GetFilename(), sz, f.cfg.MaxSingleBytes))
		}

		mtype := normaliseMediaType(a.GetContentType())

		if _, blocked := f.blocked[mtype]; blocked {
			return reject(f.name, fmt.Sprintf(
				"attachment %q has blocked MIME type %q", a.GetFilename(), mtype))
		}

		if len(f.allowed) > 0 {
			if _, ok := f.allowed[mtype]; !ok {
				return reject(f.name, fmt.Sprintf(
					"attachment %q has disallowed MIME type %q", a.GetFilename(), mtype))
			}
		}
	}

	if f.cfg.MaxTotalBytes > 0 && totalBytes > f.cfg.MaxTotalBytes {
		return reject(f.name, fmt.Sprintf(
			"total attachment size %d exceeds limit of %d bytes",
			totalBytes, f.cfg.MaxTotalBytes))
	}

	return nil
}

// normaliseMediaType strips MIME parameters so "text/plain; charset=utf-8"
// compares equal to "text/plain".
func normaliseMediaType(ct string) string {
	if ct == "" {
		return ""
	}
	mediatype, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// Fall back to lower-cased raw value with parameters stripped manually.
		if idx := strings.IndexByte(ct, ';'); idx >= 0 {
			ct = ct[:idx]
		}
		return strings.ToLower(strings.TrimSpace(ct))
	}
	return mediatype
}

// toSet converts a string slice to a set map for O(1) lookups.
func toSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = struct{}{}
	}
	return m
}
