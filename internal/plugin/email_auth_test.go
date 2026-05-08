package plugin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbaliyan/postbox/internal/plugin"
)

func TestEmailAuth_NameInitClose(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{})
	if p.Name() != "ea" {
		t.Fatalf("Name()=%q", p.Name())
	}
	ctx := context.Background()
	if err := p.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEmailAuth_SPF_Off_Passes(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "off"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "fail"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("policy=off should not reject: %v", err)
	}
}

func TestEmailAuth_SPF_Warn_Passes(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "warn"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "fail"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("policy=warn should not reject: %v", err)
	}
}

func TestEmailAuth_SPF_Reject_OnFail(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "reject"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "fail"}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("policy=reject should reject on fail")
	}
}

func TestEmailAuth_SPF_Reject_OnSoftFail(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "reject"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "softfail"}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("policy=reject should reject on softfail")
	}
}

func TestEmailAuth_SPF_Reject_PassAllowed(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "reject"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "pass"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("policy=reject should allow on pass: %v", err)
	}
}

func TestEmailAuth_SPF_Require_RejectsNone(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "require"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "none"}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("policy=require should reject when result is none")
	}
}

func TestEmailAuth_SPF_Require_PassesOnPass(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{SPFPolicy: "require"})
	draft := &fakeDraft{headers: map[string]string{"X-SPF-Result": "pass"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("policy=require should allow on pass: %v", err)
	}
}

func TestEmailAuth_DKIM_Off_Passes(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{DKIMPolicy: "off"})
	draft := &fakeDraft{headers: map[string]string{"X-DKIM-Result": "fail"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("dkim policy=off should not reject: %v", err)
	}
}

func TestEmailAuth_DKIM_Reject_OnFail(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{DKIMPolicy: "reject"})
	draft := &fakeDraft{headers: map[string]string{"X-DKIM-Result": "fail"}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("dkim policy=reject should reject on fail")
	}
}

func TestEmailAuth_DKIM_Require_RejectsNone(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{DKIMPolicy: "require"})
	draft := &fakeDraft{headers: map[string]string{"X-DKIM-Result": "none"}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("dkim policy=require should reject when result is none")
	}
}

func TestEmailAuth_DKIM_Require_PassOnPass(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{DKIMPolicy: "require"})
	draft := &fakeDraft{headers: map[string]string{"X-DKIM-Result": "pass"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("dkim policy=require should allow on pass: %v", err)
	}
}

func TestEmailAuth_BothPolicies_BothFail_RejectsOnFirst(t *testing.T) {
	p := plugin.NewEmailAuth("ea", plugin.EmailAuthConfig{
		SPFPolicy:  "reject",
		DKIMPolicy: "reject",
	})
	draft := &fakeDraft{headers: map[string]string{
		"X-SPF-Result":  "fail",
		"X-DKIM-Result": "fail",
	}}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("should reject when both SPF and DKIM fail")
	}
}
