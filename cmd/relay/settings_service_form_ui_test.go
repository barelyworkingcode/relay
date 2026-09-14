package main

// JS-module-logic coverage for the service Edit dialog's two additions:
// the capabilities editor (bug 1) and the masked environment editor (bug
// 2). Both run against the real bundle in goja, the same way every other
// settings_*_ui_test.go file does — there is no browser-driven tier in this
// repo, so this is "the JS module logic the repo's existing way."

import (
	"strings"
	"testing"
)

// TestServiceForm_CapabilitiesEditor covers the checkbox editor end to end:
// it reflects a stored service's capability set, an unchecked-by-default new
// service starts with none, and harvesting reads the checkboxes rather than
// echoing whatever was last stored.
func TestServiceForm_CapabilitiesEditor(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'relaytts', display_name:'relayTTS', command:'/bin/tts', args:[], env:{},
			 capabilities:['manifest','projects']}
		];
		window.editService('relaytts');
		var html = window.renderServiceForm();

		// The DOM shim's getElementById does not parse "checked" out of a
		// rendered HTML string (there is no real DOM tree here) -- the two
		// regex checks above already cover "did the render seed the right
		// boxes"; this half covers "does harvest read the boxes", so every
		// checkbox is set explicitly to the state under test rather than
		// assumed to start from what render() produced.
		document.getElementById('svcCap_manifest').checked = true;
		document.getElementById('svcCap_projects').checked = false;
		document.getElementById('svcCap_frontend').checked = false;
		var narrowed = window.svcFormValues();

		window.newService();
		var newHtml = window.renderServiceForm();
		// Same shim limitation as above: nothing re-parses newHtml into the
		// cached checkbox objects, so the leftover checked=true from editing
		// relayTTS above has to be reset by hand to match what a freshly
		// opened, untouched New Service form's regex-verified HTML shows.
		document.getElementById('svcCap_manifest').checked = false;
		document.getElementById('svcCap_projects').checked = false;
		document.getElementById('svcCap_frontend').checked = false;
		var newDefaults = window.svcFormValues();

		document.getElementById('svcCap_manifest').checked = true;
		document.getElementById('svcCap_projects').checked = true;
		document.getElementById('svcCap_frontend').checked = true;
		var widened = window.svcFormValues();

		return JSON.stringify({
			showsSection: html.indexOf('Capabilities') >= 0,
			manifestChecked: /id="svcCap_manifest"[^>]*checked/.test(html),
			projectsChecked: /id="svcCap_projects"[^>]*checked/.test(html),
			frontendUnchecked: !/id="svcCap_frontend"[^>]*checked/.test(html),
			narrowed: JSON.stringify(narrowed.capabilities),
			newHasNoneChecked: !/svcCap_[a-z]+"[^>]*checked/.test(newHtml),
			newDefaultsEmpty: newDefaults.capabilities.length === 0,
			widened: JSON.stringify(widened.capabilities)
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"showsSection":true`, `"manifestChecked":true`, `"projectsChecked":true`,
		`"frontendUnchecked":true`, `"narrowed":"[\"manifest\"]"`,
		`"newHasNoneChecked":true`, `"newDefaultsEmpty":true`,
		`"widened":"[\"frontend\",\"manifest\",\"projects\"]"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("capabilities editor: missing %s in %s", want, got)
		}
	}
}

// TestServiceForm_EnvEditorNeverShowsOrResubmitsAStoredValue is bug 2's core
// regression test: a stored value must never appear in the rendered HTML
// (not even the "[object Object]" artefact a raw sealed Secret would
// stringify to), an untouched key harvests as null ("keep what's stored"),
// Replace harvests the typed value, and Remove drops the key from the
// payload entirely.
func TestServiceForm_EnvEditorNeverShowsOrResubmitsAStoredValue(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'svc', display_name:'Svc', command:'/bin/old', args:[],
			 env:{API_KEY:'super-secret-value', PORT:'port-secret-9999'}, capabilities:[]}
		];
		window.editService('svc');
		var html = window.renderServiceForm();
		var untouchedPayload = window.svcFormValues();

		window.svcEnvSetMode(window.state.svcEnvDraft.findIndex(function(r){return r.key==='PORT';}), 'replace');
		var replacingHtml = window.renderServiceForm();
		var portInputId = replacingHtml.indexOf('onchange="svcEnvSetValue(');
		window.svcEnvSetValue(window.state.svcEnvDraft.findIndex(function(r){return r.key==='PORT';}), '9090');
		var replacedPayload = window.svcFormValues();

		window.svcEnvRemoveRow(window.state.svcEnvDraft.findIndex(function(r){return r.key==='API_KEY';}));
		var removedPayload = window.svcFormValues();

		return JSON.stringify({
			neverShowsValue: html.indexOf('super-secret-value') < 0 && html.indexOf('port-secret-9999') < 0,
			neverShowsArtifact: html.indexOf('[object Object]') < 0,
			showsMaskedPlaceholder: html.indexOf('••••••••') >= 0,
			untouchedIsNull: untouchedPayload.env.API_KEY === null && untouchedPayload.env.PORT === null,
			replacedValue: replacedPayload.env.PORT === '9090',
			replacedOtherStillNull: replacedPayload.env.API_KEY === null,
			removedIsAbsent: !('API_KEY' in removedPayload.env),
			removedOtherSurvives: removedPayload.env.PORT === '9090'
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"neverShowsValue":true`, `"neverShowsArtifact":true`, `"showsMaskedPlaceholder":true`,
		`"untouchedIsNull":true`, `"replacedValue":true`, `"replacedOtherStillNull":true`,
		`"removedIsAbsent":true`, `"removedOtherSurvives":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env editor: missing %s in %s", want, got)
		}
	}
}

// TestServiceForm_EnvEditorAddRow covers adding a brand-new key: it never
// existed in the stored record, so it has no "keep" state at all -- it must
// harvest as an explicit value from the first render.
func TestServiceForm_EnvEditorAddRow(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [{id:'svc', display_name:'Svc', command:'/bin/old', args:[], env:{}, capabilities:[]}];
		window.editService('svc');
		document.getElementById('svcEnvNewKey').value = 'NEW_VAR';
		document.getElementById('svcEnvNewValue').value = 'new-value';
		window.svcEnvAddRow();
		var payload = window.svcFormValues();
		var html = window.renderServiceForm();
		return JSON.stringify({
			harvested: payload.env.NEW_VAR,
			shownInDraftNotMasked: html.indexOf('value="new-value"') >= 0
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{`"harvested":"new-value"`, `"shownInDraftNotMasked":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("env add row: missing %s in %s", want, got)
		}
	}
}
