// Package plugin provides mailbox.Plugin implementations for postbox.
//
// Every plugin satisfies at least mailbox.Plugin (Name/Init/Close) and
// mailbox.SendHook (BeforeSend/AfterSend). To abort a message delivery from
// BeforeSend, return any error that wraps ErrRejected using the package-private
// reject() helper. Callers can detect rejections with errors.Is(err, ErrRejected).
//
// Recommended registration order — cheapest checks first:
//
//	CrowdSec → DNSBL → AddressFilter → AttachmentFilter → SMTPSecurity →
//	EmailAuth → SpamChecker → AntiVirus → SecurityAgent
package plugin

import (
	"errors"
	"fmt"
)

// ErrRejected is the sentinel that every plugin rejection wraps.
// Test with errors.Is(err, plugin.ErrRejected).
var ErrRejected = errors.New("message rejected by plugin")

// rejection is the unexported error type used by all plugins.
// It satisfies errors.Is(_, ErrRejected) via the Is method.
type rejection struct {
	plugin string
	reason string
}

func (r *rejection) Error() string {
	return fmt.Sprintf("[%s] %s", r.plugin, r.reason)
}

// Is makes errors.Is(err, ErrRejected) return true for any *rejection.
func (r *rejection) Is(target error) bool {
	return target == ErrRejected
}

// reject returns a plugin-labelled rejection error.
func reject(pluginName, reason string) error {
	return &rejection{plugin: pluginName, reason: reason}
}
