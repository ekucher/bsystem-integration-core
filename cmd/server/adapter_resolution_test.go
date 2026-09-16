package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekucher/bsystem-integration-core/internal/adapters"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/espocrm"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/outline"
	"github.com/ekucher/bsystem-integration-core/internal/adapters/redmine"
)

// adapterFor turns the state of an integration into the answer a caller gets.
// Three states have to stay distinguishable, because the operator's next
// action differs for each: set an environment variable, upgrade the platform,
// or wait for an upstream to recover.
//
// The distinction is not cosmetic. An unconfigured integration is a supported
// deployment — docs/STAGE-ACCEPTANCE.md in bsystem-deploy invites accepting the
// CRM path with Redmine left out — so it is the state a caller meets most
// often, and the one that must not read as a fault.

// capabilityProbe is a capability no adapter implements, so it isolates the
// "configured but not capable" branch from the capabilities the real adapters
// happen to have today.
type capabilityProbe interface {
	ProbeCapabilityThatNoAdapterImplements()
}

// registerProbe puts an adapter in the registry under an id no production code
// uses. The registry refuses duplicate ids and has no removal, so each test
// takes its own id rather than sharing one and depending on test order.
func registerProbe(t *testing.T, id string, status adapters.Status) {
	t.Helper()
	err := adapterRegistry.Register(adapters.Mock{
		AdapterInfo:   adapters.Info{ID: id, Name: id, Version: "0", Status: status},
		AdapterHealth: adapters.Health{Status: status},
	})
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func resolveFailure(t *testing.T, id string) (int, map[string]string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	if _, ok := adapterFor[capabilityProbe](recorder, id, "Probe"); ok {
		t.Fatalf("%s resolved a capability nothing implements", id)
	}
	var failure map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("%s: the failure is not the platform's error shape: %v", id, err)
	}
	return recorder.Code, failure
}

// An integration nobody configured is registered all the same: a disabled
// placeholder stands in for it so /health and /adapters can report it as
// absent rather than omit it. That placeholder implements no capability, so a
// resolver that checks the capability first reports every unconfigured
// integration as one the product cannot do — sending an operator to look for a
// missing feature instead of a missing environment variable.
func TestAnUnconfiguredIntegrationIsReportedAsUnconfigured(t *testing.T) {
	registerProbe(t, "probe-disabled", adapters.StatusDisabled)

	status, failure := resolveFailure(t, "probe-disabled")

	if status != http.StatusServiceUnavailable {
		t.Fatalf("answered %d, want 503", status)
	}
	if failure["code"] != "adapter_not_configured" {
		t.Errorf("code is %q, want adapter_not_configured", failure["code"])
	}
	if failure["source"] != "probe-disabled" {
		t.Errorf("source is %q; a caller must be able to name the missing integration", failure["source"])
	}
	if failure["error"] == "" {
		t.Error("no human-readable summary")
	}
}

// An integration that is absent from the registry entirely — no placeholder,
// because the id is not one the platform knows — is the same answer. The
// caller's situation is identical, so the code must be too.
func TestAnAbsentIntegrationIsReportedAsUnconfigured(t *testing.T) {
	status, failure := resolveFailure(t, "probe-never-registered")

	if status != http.StatusServiceUnavailable {
		t.Fatalf("answered %d, want 503", status)
	}
	if failure["code"] != "adapter_not_configured" {
		t.Errorf("code is %q, want adapter_not_configured", failure["code"])
	}
}

// A configured integration that cannot do what the endpoint needs is a
// different answer, and the only one that should read as a limit of the
// product. Configuring the integration will not fix it.
func TestAConfiguredIntegrationLackingACapabilitySaysSo(t *testing.T) {
	for _, status := range []adapters.Status{adapters.StatusReady, adapters.StatusDegraded} {
		id := "probe-" + string(status)
		registerProbe(t, id, status)

		code, failure := resolveFailure(t, id)

		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s answered %d, want 503", id, code)
		}
		if failure["code"] != "capability_unsupported" {
			t.Errorf("%s: code is %q, want capability_unsupported", id, failure["code"])
		}
		if failure["source"] != id {
			t.Errorf("%s: source is %q", id, failure["source"])
		}
	}
}

// A degraded integration is configured. Its request must be attempted and
// allowed to fail as an upstream failure, rather than refused up front as a
// configuration problem — otherwise a transient upstream outage is reported to
// an operator as something they misconfigured.
func TestADegradedIntegrationIsNotTreatedAsUnconfigured(t *testing.T) {
	_, failure := resolveFailure(t, "probe-"+string(adapters.StatusDegraded))

	if failure["code"] == "adapter_not_configured" {
		t.Error("a degraded integration is configured; reporting it as unconfigured sends the operator to the wrong place")
	}
}

// The three real adapters satisfy every capability the endpoints ask of them.
// This is a compile-time assertion: if an adapter loses a method, this file
// stops building, rather than the endpoint quietly starting to answer
// capability_unsupported at runtime — which would look like a configuration
// problem to whoever hit it.
var (
	_ crmReader            = (*espocrm.Client)(nil)
	_ crmDetailReader      = (*espocrm.Client)(nil)
	_ projectReader        = (*redmine.Client)(nil)
	_ projectDetailReader  = (*redmine.Client)(nil)
	_ documentReader       = (*outline.Client)(nil)
	_ documentDetailReader = (*outline.Client)(nil)
)

// The disabled placeholder must remain a placeholder. If it ever grew the
// methods of a capability, an unconfigured integration would start answering
// 200 with an empty collection, which tells an operator the platform knows of
// no records when nobody has told it where to look.
func TestTheDisabledPlaceholderImplementsNoCapability(t *testing.T) {
	placeholder := disabledAdapter("espocrm", "EspoCRM", []string{"clients.read"})

	var adapter adapters.Adapter = placeholder
	if _, ok := adapter.(crmReader); ok {
		t.Error("the disabled placeholder implements crmReader; an unconfigured integration would answer with data it does not have")
	}
	if _, ok := adapter.(crmDetailReader); ok {
		t.Error("the disabled placeholder implements crmDetailReader")
	}
	if placeholder.Info().Status != adapters.StatusDisabled {
		t.Errorf("the placeholder reports %q, want disabled", placeholder.Info().Status)
	}
	if got := placeholder.Health(context.Background()).Status; got != adapters.StatusDisabled {
		t.Errorf("the placeholder's health reports %q, want disabled", got)
	}
}
