package main

// JS-module-logic coverage for the service Edit dialog's Allowed Models
// editor (R-M1d): the capabilities vocabulary now includes models/model_host,
// the row editor appears only alongside the models capability, add/remove
// rows harvest into the right payload, and a form that never shows the
// section omits allowed_models entirely so an update leaves the stored grant
// alone. Same goja-against-the-real-bundle pattern as
// settings_service_form_ui_test.go.

import (
	"strings"
	"testing"
)

// TestServiceForm_CapabilitiesListIncludesModelCapabilities pins scope item 1:
// the checkbox section lists all five known capabilities, from the one JS
// array (serviceCapabilityNames) rather than a second hardcoded list.
func TestServiceForm_CapabilitiesListIncludesModelCapabilities(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'svc', display_name:'Svc', command:'/bin/x', args:[], env:{}, capabilities:[]}
		];
		window.editService('svc');
		var html = window.renderServiceForm();
		return JSON.stringify({
			hasFrontend: html.indexOf('id="svcCap_frontend"') >= 0,
			hasManifest: html.indexOf('id="svcCap_manifest"') >= 0,
			hasProjects: html.indexOf('id="svcCap_projects"') >= 0,
			hasModels: html.indexOf('id="svcCap_models"') >= 0,
			hasModelHost: html.indexOf('id="svcCap_model_host"') >= 0
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"hasFrontend":true`, `"hasManifest":true`, `"hasProjects":true`,
		`"hasModels":true`, `"hasModelHost":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("capabilities list: missing %s in %s", want, got)
		}
	}
}

// TestServiceForm_AllowedModelsShownOnlyWithModelsCapability covers the
// section's visibility: present for a stored service holding models,
// absent for one that does not (including a brand-new service, which always
// starts with no capabilities).
func TestServiceForm_AllowedModelsShownOnlyWithModelsCapability(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'tts', display_name:'TTS', command:'/bin/tts', args:[], env:{},
			 capabilities:['models'], allowed_models:['vCode']},
			{id:'other', display_name:'Other', command:'/bin/x', args:[], env:{},
			 capabilities:['manifest']}
		];
		window.editService('tts');
		var withModels = window.renderServiceForm();

		window.editService('other');
		var withoutModels = window.renderServiceForm();

		window.newService();
		var freshForm = window.renderServiceForm();

		return JSON.stringify({
			showsSection: withModels.indexOf('Allowed Models') >= 0,
			showsEmptyRule: withModels.indexOf('NO models') >= 0,
			showsStoredId: withModels.indexOf('value="vCode"') >= 0,
			hidesForOther: withoutModels.indexOf('Allowed Models') < 0,
			hidesForNew: freshForm.indexOf('Allowed Models') < 0
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"showsSection":true`, `"showsEmptyRule":true`, `"showsStoredId":true`,
		`"hidesForOther":true`, `"hidesForNew":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("allowed models visibility: missing %s in %s", want, got)
		}
	}
}

// TestServiceForm_AllowedModelsAddRemoveRows covers the row editor end to
// end: adding a row appends the typed id (including the literal wildcard
// "*"), and removing a row drops it -- both read back through svcFormValues,
// the same harvest saveServiceEdit/addService use.
func TestServiceForm_AllowedModelsAddRemoveRows(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'tts', display_name:'TTS', command:'/bin/tts', args:[], env:{},
			 capabilities:['models'], allowed_models:['vCode']}
		];
		window.editService('tts');
		// The DOM shim does not parse "checked" out of rendered HTML into a
		// real element (settings_service_form_ui_test.go's own capabilities
		// test notes the same limitation) -- svcFormValues reads the
		// checkbox live, so the harvest half of this test sets it explicitly
		// to what a stored 'models' capability renders as checked.
		document.getElementById('svcCap_models').checked = true;

		document.getElementById('svcModelNewId').value = 'omlx/Chat';
		window.svcModelAddRow();
		var afterAdd = window.svcFormValues();

		document.getElementById('svcModelNewId').value = '*';
		window.svcModelAddRow();
		var afterWildcardAdd = window.svcFormValues();

		window.svcModelRemoveRow(0); // drop the original 'vCode' row
		var afterRemove = window.svcFormValues();

		return JSON.stringify({
			afterAdd: JSON.stringify(afterAdd.allowedModels),
			afterWildcardAdd: JSON.stringify(afterWildcardAdd.allowedModels),
			afterRemove: JSON.stringify(afterRemove.allowedModels)
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{
		`"afterAdd":"[\"vCode\",\"omlx/Chat\"]"`,
		`"afterWildcardAdd":"[\"vCode\",\"omlx/Chat\",\"*\"]"`,
		`"afterRemove":"[\"omlx/Chat\",\"*\"]"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("add/remove rows: missing %s in %s", want, got)
		}
	}
}

// TestServiceForm_AllowedModelsBlankEntryIgnored covers svcModelAddRow's
// input guard: an empty or whitespace-only typed id adds no row, the same
// "nothing to add" convenience svcEnvAddRow already gives a blank key.
func TestServiceForm_AllowedModelsBlankEntryIgnored(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'tts', display_name:'TTS', command:'/bin/tts', args:[], env:{},
			 capabilities:['models'], allowed_models:[]}
		];
		window.editService('tts');
		document.getElementById('svcCap_models').checked = true;
		document.getElementById('svcModelNewId').value = '   ';
		window.svcModelAddRow();
		var v = window.svcFormValues();
		return JSON.stringify({ count: v.allowedModels.length });
	})()`

	got := evalString(t, vm, script)
	if !strings.Contains(got, `"count":0`) {
		t.Errorf("blank entry should not add a row: %s", got)
	}
}

// TestServiceForm_AllowedModelsOmittedWhenSectionNeverShown is the field's
// core regression test, mirroring TestProjectFormTemplatesReadOnly's
// "omit means leave it alone" pattern: a service that never held (or was
// never given) the models capability harvests with no allowedModels key at
// all, so JSON.stringify drops it and saveServiceEdit/addService never send
// allowed_models on the wire -- an update must not be able to clear a grant
// it never mentions.
func TestServiceForm_AllowedModelsOmittedWhenSectionNeverShown(t *testing.T) {
	vm := newAppVM(t)

	script := `(function(){
		window.state.services = [
			{id:'plain', display_name:'Plain', command:'/bin/x', args:[], env:{},
			 capabilities:['manifest']}
		];
		window.editService('plain');
		var v = window.svcFormValues();
		return JSON.stringify({
			keyAbsent: !('allowedModels' in v),
			wireOmits: JSON.stringify({allowed_models: v.allowedModels}).indexOf('allowed_models') < 0
		});
	})()`

	got := evalString(t, vm, script)
	for _, want := range []string{`"keyAbsent":true`, `"wireOmits":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("omitted allowed models: missing %s in %s", want, got)
		}
	}
}
